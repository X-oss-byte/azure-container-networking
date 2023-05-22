package dataplane

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/pkg/dataplane/ipsets"
	"github.com/Azure/azure-container-networking/npm/pkg/dataplane/policies"
	"github.com/Azure/azure-container-networking/npm/util"
	npmerrors "github.com/Azure/azure-container-networking/npm/util/errors"
	"k8s.io/klog"
)

const (
	reconcileDuration = time.Duration(5 * time.Minute)

	contextBackground = "BACKGROUND"
	contextApplyDP    = "APPLY-DP"
	contextAddNetPol  = "ADD-NETPOL"
	contextDelNetPol  = "DEL-NETPOL"
)

var ErrInvalidApplyConfig = errors.New("invalid apply config")

type PolicyMode string

// TODO put NodeName in Config?
type Config struct {
	ApplyInBackground bool
	ApplyMaxBatches   int
	ApplyInterval     time.Duration
	*ipsets.IPSetManagerCfg
	*policies.PolicyManagerCfg
}

type endpointCache struct {
	sync.Mutex
	cache map[string]*npmEndpoint
}

func newEndpointCache() *endpointCache {
	return &endpointCache{cache: make(map[string]*npmEndpoint)}
}

type applyInfo struct {
	sync.Mutex
	numBatches int
}

type netPolInfo struct {
	sync.Mutex
	numBatches               int
	toDeleteNetPolReferences map[string][]string
}

type DataPlane struct {
	*Config
	applyInBackground    bool
	iptablesInBackground bool
	policyMgr            *policies.PolicyManager
	ipsetMgr             *ipsets.IPSetManager
	networkID            string
	nodeName             string
	// endpointCache stores all endpoints of the network (including off-node)
	// Key is PodIP
	endpointCache  *endpointCache
	ioShim         *common.IOShim
	updatePodCache *updatePodCache
	endpointQuery  *endpointQuery
	applyInfo      *applyInfo
	netPolInfo     *netPolInfo
	stopChannel    <-chan struct{}
}

func NewDataPlane(nodeName string, ioShim *common.IOShim, cfg *Config, stopChannel <-chan struct{}) (*DataPlane, error) {
	metrics.InitializeAll()
	if util.IsWindowsDP() {
		klog.Infof("[DataPlane] enabling AddEmptySetToLists for Windows")
		cfg.IPSetManagerCfg.AddEmptySetToLists = true
	}

	dp := &DataPlane{
		Config:    cfg,
		policyMgr: policies.NewPolicyManager(ioShim, cfg.PolicyManagerCfg),
		ipsetMgr:  ipsets.NewIPSetManager(cfg.IPSetManagerCfg, ioShim),
		// networkID is set when initializing Windows dataplane
		networkID:     "",
		endpointCache: newEndpointCache(),
		nodeName:      nodeName,
		ioShim:        ioShim,
		endpointQuery: new(endpointQuery),
		applyInfo:     &applyInfo{},
		netPolInfo: &netPolInfo{
			toDeleteNetPolReferences: make(map[string][]string),
		},
		stopChannel: stopChannel,
	}

	dp.iptablesInBackground = cfg.IPTablesInBackground && !util.IsWindowsDP()
	if dp.iptablesInBackground {
		klog.Infof("[DataPlane] dataplane configured to run iptables in background every %v or every %d calls to AddPolicy() or RemovePolicy()", dp.IPTablesInterval, dp.IPTablesMaxBatches)
	} else {
		klog.Info("[DataPlane] dataplane configured to NOT run iptables in background")
	}

	// do not let Linux apply in background
	dp.applyInBackground = cfg.ApplyInBackground && util.IsWindowsDP()

	if dp.applyInBackground {
		dp.updatePodCache = newUpdatePodCache(cfg.ApplyMaxBatches)
	} else {
		dp.updatePodCache = newUpdatePodCache(1)
	}

	if dp.applyInBackground {
		klog.Infof("[DataPlane] dataplane configured to apply in background every %v or every %d calls to ApplyDataPlane()", dp.ApplyInterval, dp.ApplyMaxBatches)
		if dp.ApplyMaxBatches <= 0 || dp.ApplyInterval == 0 {
			return nil, ErrInvalidApplyConfig
		}
	} else {
		klog.Info("[DataPlane] dataplane configured to NOT apply in background")
	}

	err := dp.BootupDataplane()
	if err != nil {
		klog.Errorf("Failed to reset dataplane: %v", err)
		return nil, err
	}
	return dp, nil
}

// BootupDataplane cleans the NPM sets and policies in the dataplane and performs initialization.
func (dp *DataPlane) BootupDataplane() error {
	// NOTE: used to create an all-namespaces set, but there's no need since it will be created by the control plane
	return dp.bootupDataPlane() //nolint:wrapcheck // unnecessary to wrap error
}

// RunPeriodicTasks runs periodic tasks. Should only be called once.
func (dp *DataPlane) RunPeriodicTasks() {
	go func() {
		ticker := time.NewTicker(reconcileDuration)
		defer ticker.Stop()

		for {
			select {
			case <-dp.stopChannel:
				return
			case <-ticker.C:
				// send the heartbeat log in another go routine in case it takes a while
				go metrics.SendHeartbeatWithNumPolicies()

				// locks ipset manager
				dp.ipsetMgr.Reconcile()

				// in Windows, does nothing
				// in Linux, locks policy manager but can be interrupted
				dp.policyMgr.Reconcile()
			}
		}
	}()

	if dp.iptablesInBackground {
		go func() {
			ticker := time.NewTicker(dp.IPTablesInterval)
			defer ticker.Stop()

			for {
				select {
				case <-dp.stopChannel:
					return
				case <-ticker.C:
					// Choose to keep netPolInfo locked while running iptables-restore within reconcileDirtyNetPolsNow.
					// We are only blocking the NetPol controller, which wouldn't be able to use the PolicyManager while it's reconciling.
					// Technically, NetPol controller could be adding IPSets during this lock, but this is very fast.
					// Prefering thread safety over optimized performance.
					dp.netPolInfo.Lock()
					if dp.netPolInfo.numBatches == 0 {
						dp.netPolInfo.Unlock()
						continue
					}
					_ = dp.reconcileDirtyNetPolsNow(contextBackground)
					dp.netPolInfo.Unlock()
				}
			}
		}()
	}

	if !dp.applyInBackground {
		return
	}

	go func() {
		ticker := time.NewTicker(dp.ApplyInterval)
		defer ticker.Stop()

		for {
			select {
			case <-dp.stopChannel:
				return
			case <-ticker.C:
				dp.applyInfo.Lock()
				numBatches := dp.applyInfo.numBatches
				dp.applyInfo.Unlock()
				if numBatches == 0 {
					continue
				}

				if err := dp.applyDataPlaneNow(contextBackground); err != nil {
					klog.Errorf("[DataPlane] failed to apply dataplane in background: %v", err)
					metrics.SendErrorLogAndMetric(util.DaemonDataplaneID, "[DataPlane] failed to apply dataplane in background: %v", err)
				}
			}
		}
	}()
}

func (dp *DataPlane) GetIPSet(setName string) *ipsets.IPSet {
	return dp.ipsetMgr.GetIPSet(setName)
}

// CreateIPSets takes in a set object and updates local cache with this set
func (dp *DataPlane) CreateIPSets(setMetadata []*ipsets.IPSetMetadata) {
	dp.ipsetMgr.CreateIPSets(setMetadata)
}

// DeleteSet checks for members and references of the given "set" type ipset
// if not used then will delete it from cache
func (dp *DataPlane) DeleteIPSet(setMetadata *ipsets.IPSetMetadata, forceDelete util.DeleteOption) {
	dp.ipsetMgr.DeleteIPSet(setMetadata.GetPrefixName(), forceDelete)
}

// AddToSets takes in a list of IPSet names along with IP member
// and then updates it local cache
func (dp *DataPlane) AddToSets(setNames []*ipsets.IPSetMetadata, podMetadata *PodMetadata) error {
	err := dp.ipsetMgr.AddToSets(setNames, podMetadata.PodIP, podMetadata.PodKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding to set: %w", err)
	}

	if dp.shouldUpdatePod() && podMetadata.NodeName == dp.nodeName {
		klog.Infof("[DataPlane] Updating Sets to Add for pod key %s", podMetadata.PodKey)

		// lock updatePodCache while reading/modifying or setting the updatePod in the cache
		dp.updatePodCache.Lock()
		defer dp.updatePodCache.Unlock()

		updatePod := dp.updatePodCache.enqueue(podMetadata)
		updatePod.updateIPSetsToAdd(setNames)
	}

	return nil
}

// RemoveFromSets takes in list of setnames from which a given IP member should be
// removed and will update the local cache
func (dp *DataPlane) RemoveFromSets(setNames []*ipsets.IPSetMetadata, podMetadata *PodMetadata) error {
	err := dp.ipsetMgr.RemoveFromSets(setNames, podMetadata.PodIP, podMetadata.PodKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing from set: %w", err)
	}

	if dp.shouldUpdatePod() && podMetadata.NodeName == dp.nodeName {
		klog.Infof("[DataPlane] Updating Sets to Remove for pod key %s", podMetadata.PodKey)

		// lock updatePodCache while reading/modifying or setting the updatePod in the cache
		dp.updatePodCache.Lock()
		defer dp.updatePodCache.Unlock()

		updatePod := dp.updatePodCache.enqueue(podMetadata)
		updatePod.updateIPSetsToRemove(setNames)
	}

	return nil
}

// AddToLists takes a list name and list of sets which are to be added as members
// to given list
func (dp *DataPlane) AddToLists(listName, setNames []*ipsets.IPSetMetadata) error {
	err := dp.ipsetMgr.AddToLists(listName, setNames)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding to list: %w", err)
	}
	return nil
}

// RemoveFromList takes a list name and list of sets which are to be removed as members
// to given list
func (dp *DataPlane) RemoveFromList(listName *ipsets.IPSetMetadata, setNames []*ipsets.IPSetMetadata) error {
	err := dp.ipsetMgr.RemoveFromList(listName, setNames)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing from list: %w", err)
	}
	return nil
}

// ApplyDataPlane all the IPSet operations just update cache and update a dirty ipset structure,
// they do not change apply changes into dataplane. This function needs to be called at the
// end of IPSet operations of a given controller event, it will check for the dirty ipset list
// and accordingly makes changes in dataplane. This function helps emulate a single call to
// dataplane instead of multiple ipset operations calls ipset operations calls to dataplane
func (dp *DataPlane) ApplyDataPlane() error {
	if dp.applyInBackground {
		return dp.incrementBatchAndApplyIfNeeded(contextApplyDP)
	}

	return dp.applyDataPlaneNow(contextApplyDP)
}

func (dp *DataPlane) incrementBatchAndApplyIfNeeded(context string) error {
	dp.applyInfo.Lock()
	dp.applyInfo.numBatches++
	newCount := dp.applyInfo.numBatches
	dp.applyInfo.Unlock()

	klog.Infof("[DataPlane] [%s] new batch count: %d", context, newCount)

	if newCount >= dp.ApplyMaxBatches {
		klog.Infof("[DataPlane] [%s] applying now since reached maximum batch count: %d", context, newCount)
		return dp.applyDataPlaneNow(context)
	}

	return nil
}

func (dp *DataPlane) applyDataPlaneNow(context string) error {
	klog.Infof("[DataPlane] [ApplyDataPlane] [%s] starting to apply ipsets", context)
	err := dp.ipsetMgr.ApplyIPSets()
	if err != nil {
		return fmt.Errorf("[DataPlane] [%s] error while applying IPSets: %w", context, err)
	}
	klog.Infof("[DataPlane] [ApplyDataPlane] [%s] finished applying ipsets", context)

	if dp.applyInBackground {
		dp.applyInfo.Lock()
		dp.applyInfo.numBatches = 0
		dp.applyInfo.Unlock()
	}

	// NOTE: ideally we won't refresh Pod Endpoints if the updatePodCache is empty
	if dp.shouldUpdatePod() {
		// do not refresh endpoints if the updatePodCache is empty
		dp.updatePodCache.Lock()
		if dp.updatePodCache.isEmpty() {
			dp.updatePodCache.Unlock()
			return nil
		}
		dp.updatePodCache.Unlock()

		klog.Infof("[DataPlane] [ApplyDataPlane] [%s] refreshing endpoints before updating pods", context)

		err := dp.refreshPodEndpoints()
		if err != nil {
			metrics.SendErrorLogAndMetric(util.DaemonDataplaneID, "[DataPlane] failed to refresh endpoints while updating pods. err: [%s]", err.Error())
			// return as success since this can be retried irrespective of other operations
			return nil
		}

		klog.Infof("[DataPlane] [ApplyDataPlane] [%s] refreshed endpoints", context)

		// lock updatePodCache while driving goal state to kernel
		// prevents another ApplyDataplane call from updating the same pods
		dp.updatePodCache.Lock()
		defer dp.updatePodCache.Unlock()

		klog.Infof("[DataPlane] [ApplyDataPlane] [%s] starting to update pods", context)
		for !dp.updatePodCache.isEmpty() {
			pod := dp.updatePodCache.dequeue()
			if pod == nil {
				// should never happen because of isEmpty check above and lock on updatePodCache
				metrics.SendErrorLogAndMetric(util.DaemonDataplaneID, "[DataPlane] failed to dequeue pod while applying the dataplane")
				// break to avoid infinite loop (something weird happened since isEmpty returned false above)
				break
			}

			if err := dp.updatePod(pod); err != nil {
				// move on to the next and later return as success since this can be retried irrespective of other operations
				metrics.SendErrorLogAndMetric(util.DaemonDataplaneID, "failed to update pod while applying the dataplane. key: [%s], err: [%s]", pod.PodKey, err.Error())
				dp.updatePodCache.requeue(pod)
				continue
			}
		}

		klog.Infof("[DataPlane] [ApplyDataPlane] [%s] finished updating pods", context)
	}
	return nil
}

// AddPolicy takes in a translated NPMNetworkPolicy object and applies on dataplane
func (dp *DataPlane) AddPolicy(policy *policies.NPMNetworkPolicy) error {
	klog.Infof("[DataPlane] Add Policy called for %s", policy.PolicyKey)

	// Create and add references for Selector IPSets first
	err := dp.createIPSetsAndReferences(policy.AllPodSelectorIPSets(), policy.PolicyKey, ipsets.SelectorType)
	if err != nil {
		klog.Infof("[DataPlane] error while adding Selector IPSet references: %s", err.Error())
		return fmt.Errorf("[DataPlane] error while adding Selector IPSet references: %w", err)
	}

	// Create and add references for Rule IPSets
	err = dp.createIPSetsAndReferences(policy.RuleIPSets, policy.PolicyKey, ipsets.NetPolType)
	if err != nil {
		klog.Infof("[DataPlane] error while adding Rule IPSet references: %s", err.Error())
		return fmt.Errorf("[DataPlane] error while adding Rule IPSet references: %w", err)
	}

	err = dp.applyDataPlaneNow(contextAddNetPol)
	if err != nil {
		return err
	}

	endpointList, err := dp.getEndpointsToApplyPolicy(policy)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while getting endpoints to apply policy after applying dataplane: %w", err)
	}

	err = dp.policyMgr.AddPolicy(policy, endpointList)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while adding policy: %w", err)
	}

	return dp.incrementBatchAndReconcileDirtyNetPolsIfNeeded(contextAddNetPol)
}

// RemovePolicy takes in network policyKey (namespace/name of network policy) and removes it from dataplane and cache
func (dp *DataPlane) RemovePolicy(policyKey string) error {
	klog.Infof("[DataPlane] Remove Policy called for %s", policyKey)
	// because policy Manager will remove from policy from cache
	// keep a local copy to remove references for ipsets
	policy, ok := dp.policyMgr.GetPolicy(policyKey)
	endpoints := make(map[string]string, len(policy.PodEndpoints))

	for podIP, endpointID := range policy.PodEndpoints {
		endpoints[podIP] = endpointID
	}

	if !ok {
		klog.Infof("[DataPlane] Policy %s is not found. Might been deleted already", policyKey)
		return nil
	}

	// Use the endpoint list saved in cache for this network policy to remove
	err := dp.policyMgr.RemovePolicy(policy.PolicyKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while removing policy: %w", err)
	}

	if dp.shouldUpdatePod() {

		dp.endpointCache.Lock()

		for podIP := range endpoints {
			// if the endpoint is not in the policy's endpoint list, delete policy reference from cache
			if _, ok := policy.PodEndpoints[podIP]; !ok {
				// check if the endpoint is in the cache
				if endpoint, ok := dp.endpointCache.cache[podIP]; ok {
					delete(endpoint.netPolReference, policyKey)
				}
			}
		}

		dp.endpointCache.Unlock()

	}

	// Remove references for Rule IPSets first
	err = dp.deleteIPSetsAndReferences(policy.RuleIPSets, policy.PolicyKey, ipsets.NetPolType)
	if err != nil {
		return err
	}

	// Remove references for Selector IPSets
	err = dp.deleteIPSetsAndReferences(policy.AllPodSelectorIPSets(), policy.PolicyKey, ipsets.SelectorType)
	if err != nil {
		return err
	}

	if err := dp.applyDataPlaneNow(contextApplyDP); err != nil {
		return err
	}

	return dp.incrementBatchAndReconcileDirtyNetPolsIfNeeded(contextDelNetPol)
}

// UpdatePolicy takes in updated policy object, calculates the delta and applies changes
// onto dataplane accordingly
func (dp *DataPlane) UpdatePolicy(policy *policies.NPMNetworkPolicy) error {
	klog.Infof("[DataPlane] Update Policy called for %s", policy.PolicyKey)
	ok := dp.policyMgr.PolicyExists(policy.PolicyKey)
	if !ok {
		klog.Infof("[DataPlane] Policy %s is not found.", policy.PolicyKey)
		return dp.AddPolicy(policy)
	}

	// TODO it would be ideal to calculate a diff of policies
	// and remove/apply only the delta of IPSets and policies

	// Taking the easy route here, delete existing policy
	err := dp.RemovePolicy(policy.PolicyKey)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while updating policy: %w", err)
	}
	// and add the new updated policy
	err = dp.AddPolicy(policy)
	if err != nil {
		return fmt.Errorf("[DataPlane] error while updating policy: %w", err)
	}
	return nil
}

func (dp *DataPlane) GetAllIPSets() map[string]string {
	return dp.ipsetMgr.GetAllIPSets()
}

// GetAllPolicies is deprecated and only used in the goalstateprocessor, which is deprecated
func (dp *DataPlane) GetAllPolicies() []string {
	return nil
}

func (dp *DataPlane) createIPSetsAndReferences(sets []*ipsets.TranslatedIPSet, netpolName string, referenceType ipsets.ReferenceType) error {
	// Create IPSets first along with reference updates
	npmErrorString := npmerrors.AddSelectorReference
	if referenceType == ipsets.NetPolType {
		npmErrorString = npmerrors.AddNetPolReference
	}
	for _, set := range sets {
		dp.ipsetMgr.CreateIPSets([]*ipsets.IPSetMetadata{set.Metadata})
		err := dp.ipsetMgr.AddReference(set.Metadata, netpolName, referenceType)
		if err != nil {
			return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to add reference with err: %s", err.Error()))
		}
	}

	// Check if any list sets are provided with members to add
	for _, set := range sets {
		// Check if any CIDR block IPSets needs to be applied
		setType := set.Metadata.Type
		if setType == ipsets.CIDRBlocks {
			// ipblock can have either cidr (CIDR in IPBlock) or "cidr + " " (space) + nomatch" (Except in IPBlock)
			// (TODO) need to revise it for windows
			for _, ipblock := range set.Members {
				err := dp.ipsetMgr.AddToSets([]*ipsets.IPSetMetadata{set.Metadata}, ipblock, "")
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to AddToSet in addIPSetReferences with err: %s", err.Error()))
				}
			}
		} else if setType == ipsets.NestedLabelOfPod && len(set.Members) > 0 {
			// Check if any 2nd level IPSets are generated by Controller with members
			// Apply members to the list set
			err := dp.ipsetMgr.AddToLists([]*ipsets.IPSetMetadata{set.Metadata}, ipsets.GetMembersOfTranslatedSets(set.Members))
			if err != nil {
				return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to AddToList in addIPSetReferences with err: %s", err.Error()))
			}
		}
	}

	return nil
}

func (dp *DataPlane) deleteIPSetsAndReferences(sets []*ipsets.TranslatedIPSet, netpolName string, referenceType ipsets.ReferenceType) error {
	tmp := "TMP" + netpolName
	for _, set := range sets {
		prefixName := set.Metadata.GetPrefixName()

		if dp.iptablesInBackground {
			// add temporary reference so that we don't delete the IPSet while it's in an iptables rule
			if err := dp.ipsetMgr.AddReference(set.Metadata, tmp, referenceType); err != nil {
				klog.Infof("[DataPlane] ignoring add temporary reference on non-existent set. ipset: %s. netpol: %s. referenceType: %s", prefixName, netpolName, referenceType)
				continue
			}
			dp.netPolInfo.toDeleteNetPolReferences[prefixName] = append(dp.netPolInfo.toDeleteNetPolReferences[prefixName], netpolName)
		}

		if err := dp.ipsetMgr.DeleteReference(prefixName, netpolName, referenceType); err != nil {
			// with current implementation of DeleteReference(), err will be ipsets.ErrSetDoesNotExist
			klog.Infof("[DataPlane] ignoring delete reference on non-existent set. ipset: %s. netpol: %s. referenceType: %s", prefixName, netpolName, referenceType)
		}
	}

	npmErrorString := npmerrors.DeleteSelectorReference
	if referenceType == ipsets.NetPolType {
		npmErrorString = npmerrors.DeleteNetPolReference
	}

	// Check if any list sets are provided with members to delete
	// NOTE: every translated member will be deleted, even if the member is part of the same set in another policy
	// see the definition of TranslatedIPSet for how to avoid this situation
	for _, set := range sets {
		// Check if any CIDR block IPSets needs to be applied
		setType := set.Metadata.Type
		if setType == ipsets.CIDRBlocks {
			// ipblock can have either cidr (CIDR in IPBlock) or "cidr + " " (space) + nomatch" (Except in IPBlock)
			// (TODO) need to revise it for windows
			for _, ipblock := range set.Members {
				err := dp.ipsetMgr.RemoveFromSets([]*ipsets.IPSetMetadata{set.Metadata}, ipblock, "")
				if err != nil {
					return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to RemoveFromSet in deleteIPSetReferences with err: %s", err.Error()))
				}
			}
		} else if set.Metadata.GetSetKind() == ipsets.ListSet && len(set.Members) > 0 {
			// Delete if any 2nd level IPSets are generated by Controller with members
			err := dp.ipsetMgr.RemoveFromList(set.Metadata, ipsets.GetMembersOfTranslatedSets(set.Members))
			if err != nil {
				return npmerrors.Errorf(npmErrorString, false, fmt.Sprintf("[DataPlane] failed to RemoveFromList in deleteIPSetReferences with err: %s", err.Error()))
			}
		}
	}
	return nil
}

// reconcileDirtyNetPolsNow must be called while holding the netPolInfo Lock
func (dp *DataPlane) reconcileDirtyNetPolsNow(context string) error {
	klog.Infof("[DataPlane] [%s] reconciling dirty NetPols", context)
	if err := dp.policyMgr.ReconcileDirtyNetPols(); err != nil {
		metrics.SendErrorLogAndMetric(util.IptmID, "[DataPlane] [%s] failed to reconcile dirty network policies due to %s", context, err.Error())
		klog.Error("[DataPlane] [%s] failed to reconcile dirty network policies. err: %s", context, err.Error)
		return fmt.Errorf("[DataPlane] [%s] failed to reconcile dirty network policies. err: %w", context, err)
	}

	dp.netPolInfo.numBatches = 0

	// remove all temporary references after successfully reconciling dirty netpols
	for policyKey, ipsetNames := range dp.netPolInfo.toDeleteNetPolReferences {
		tmp := "TMP" + policyKey
		for _, ipsetName := range ipsetNames {
			if err := dp.ipsetMgr.DeleteReference(ipsetName, tmp, ipsets.NetPolType); err != nil {
				// with current implementation of DeleteReference(), err will be ipsets.ErrSetDoesNotExist
				klog.Infof("[DataPlane] [%s] ignoring delete reference on non-existent set. ipset: %s. netpol: %s. referenceType: %s", context, ipsetName, policyKey, ipsets.NetPolType)
			}
		}
	}

	dp.netPolInfo.toDeleteNetPolReferences = make(map[string][]string)

	return nil
}

func (dp *DataPlane) incrementBatchAndReconcileDirtyNetPolsIfNeeded(context string) error {
	if !dp.iptablesInBackground {
		return nil
	}

	// Choose to keep netPolInfo locked while running iptables-restore within reconcileDirtyNetPolsNow.
	// We are not blocking any thread but the background iptables thread, which would run the same command anyways
	dp.netPolInfo.Lock()
	defer dp.netPolInfo.Unlock()
	dp.netPolInfo.numBatches++
	newCount := dp.netPolInfo.numBatches

	klog.Infof("[DataPlane] [%s] new netpol batch count: %d", context, newCount)

	if newCount >= dp.IPTablesMaxBatches {
		klog.Infof("[DataPlane] [%s] applying now since reached maximum batch count: %d", context, newCount)
		return dp.reconcileDirtyNetPolsNow(context)
	}

	return nil
}

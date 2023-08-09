# PodNetwork CRDs

This CRD is added to enable SWIFT multitenancy – which will be watched and managed by the MT-DNC-RC controller.

PodNetwork objects need to be created by Orchestrator RP in the subnet delegation flow (see Scenarios).
These represent a Cx subnet already delegated by the customer to the Orchestrator RP and locked with a Service Association Link (SAL) on NRP.

Orchestrator RP will map the Swift MT deployments to the PN object, and the subnet to use for IP allocations, through labels on the pod spec pointing to this object identifier.

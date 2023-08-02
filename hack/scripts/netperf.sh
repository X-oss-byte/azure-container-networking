#!/bin/bash

netperf_pod=$(kubectl get pods -l app=container6 -o wide)
target_pod=$(echo $netperf_pod | cut -d" " -f 1)
target_pod_ip=$(kubectl get pod "$target_pod" -o jsonpath='{.status.podIP}')
diff_vm_pod=$netperf_pod
kubectl exec -it $target_pod -- netserver

#netperf on different vm pod
iteration=10
while [ $iteration -ge 0 ]
do
    echo "============ Iteration $iteration ===============" 
    kubectl exec -it $diff_vm_pod -- netperf -H $target_pod_ip -l 30 -t TCP_STREAM >> "netperf/diff_vm_iteration_$iteration.log"
    echo "==============================="
    sleep 5s
    iteration=$((iteration-1))
done

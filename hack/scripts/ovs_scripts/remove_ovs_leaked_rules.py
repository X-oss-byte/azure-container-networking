import subprocess
import re
import os

# step 1: get ovs-dpctl show out to make sure which ports are being used
try:
    ovsDPCtlShow = subprocess.Popen(['ovs-dpctl', 'show'],
                            stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT)
except subprocess.CalledProcessError:
    print("failed to execute ovs-dpctl show command")
    os.Exit(1)

stdout = ovsDPCtlShow.communicate()

usedPortList = re.findall("port (\d+)", str(stdout))

# Step 2: Check ovs flows dumps
try:
    ovsDumpFlows = subprocess.Popen(['ovs-ofctl', 'dump-flows', 'azure0'],
                            stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT)
except subprocess.CalledProcessError:
    print("failed to execute ovs-ofctl dump-flows command")
    os.Exit(1)

stdout = ovsDumpFlows.communicate()
allPortList = re.findall("in_port=(\d+)", str(stdout))

unUsedPortList = [port for port in allPortList if port not in usedPortList]
# Step 3: delete leaked rules
# only use unused ports
for port in unUsedPortList:
    deleteCommand = f"ovs-ofctl del-flows azure0 ip,in_port={port}"
    try:
        os.system(deleteCommand)
    except:
        print(f"delete command {deleteCommand} does not work")
        os.Exit(1)
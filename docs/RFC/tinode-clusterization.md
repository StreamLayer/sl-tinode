# Tinode cluster

## Objective

Deploy Tinode cluster in Kubernetes.

---
## Solution

* According to https://github.com/tinode/chat/blob/master/INSTALL.md#running-a-cluster each cluster node must have unique `node_name`.

* K8s environment requirement - StatefulSet.

Example by offical doc:

###  Running a Cluster

- Install and run the database, run DB initializer, unpack JS files, and link or copy template directory as described in the previous section. Both MySQL and RethinkDB supports [cluster](https://www.mysql.com/products/cluster/) [mode](https://www.rethinkdb.com/docs/start-a-server/#a-rethinkdb-cluster-using-multiple-machines). You may consider it for added resiliency.

- Cluster expects at least two nodes. A minimum of three nodes is recommended.

- The following section configures the cluster.

```
	"cluster_config": {
		// Name of the current node.
		"self": "",
		// List of all cluster nodes, including the current one.
		"nodes": [
			{"name": "one", "addr":"localhost:12001"},
			{"name": "two", "addr":"localhost:12002"},
			{"name": "three", "addr":"localhost:12003"}
		],
		// Configuration of failover feature. Don't change.
		"failover": {
			"enabled": true,
			"heartbeat": 100,
			"vote_after": 8,
			"node_fail_after": 16
		}
	}
```
* `self` is the name of the current node. Generally it's more convenient to specify the name of the current node at the command line using `cluster_self` option. Command line value overrides the config file value. If the value is not provided either in the config file or through the command line, the clustering is disabled.
* `nodes` defines individual cluster nodes. The sample defines three nodes named `one`, `two`, and `tree` running at the localhost at the specified cluster communication ports. Cluster addresses don't need to be exposed to the outside world.
* `failover` is an experimental feature which migrates topics from failed cluster nodes keeping them accessible:
  * `enabled` turns on failover mode; failover mode requires at least three nodes in the cluster.
  * `heartbeat` interval in milliseconds between heartbeats sent by the leader node to follower nodes to ensure they are accessible.
  * `vote_after` number of failed heartbeats before a new leader node is elected.
  * `node_fail_after` number of heartbeats that a follower node misses before it's considered to be down.

If you are testing the cluster with all nodes running on the same host, you also must override the `listen` and `grpc_listen` ports. Here is an example for launching two cluster nodes from the same host using the same config file:
```
$GOPATH/bin/tinode -config=./tinode.conf -static_data=./webapp/ -listen=:6060 -grpc_listen=:6080 -cluster_self=one &
$GOPATH/bin/tinode -config=./tinode.conf -static_data=./webapp/ -listen=:6061 -grpc_listen=:6081 -cluster_self=two &
```
A bash script [run-cluster.sh](../../server/run-cluster.sh) may be found useful.


By https://github.com/StreamLayer/sl-infrastructure/blob/staging/manifests/apps/chat/tinode/tinode-config.yaml#L306-L325 section has to be dynamically generated part of config, like so:

```
        // Cluster-mode configuration.
        "cluster_config": {
          // Name of this node. Can be assigned from the command line.
          // Empty string disables clustering.
          "self": process.env.HOSTNAME,
          // naive way to set cluster nodes
          {{ if gt (int .Values.replicaCount) 1 }}
          {{ $node_name := include "microfleet.fullname" $ }}
          // List of available nodes.
          "nodes": [
            {{- range $count, $e := until (int .Values.replicaCount) -}}
            {{ $instance_name := printf "%s-%d" $node_name $count }}
            {"name": "{{- $instance_name -}}", "addr":"{{- $instance_name -}}:12001"},
            {{- end }}
          ],
          {{ else }}
          "nodes": [],
          {{ end }}
          // Failover config.
          "failover": {
            // Failover is enabled.
            "enabled": true,
            // Time in milliseconds between heartbeats.
            "heartbeat": 100,
            // Initiate leader election when the leader is not available for this many heartbeats.
            "vote_after": 8,
            // Consider node failed when it missed this many heartbeats.
            "node_fail_after": 16
          }
        },
```
ref: https://github.com/StreamLayer/sl-infrastructure/commit/7b52e433bef0550982e6882a6b35d36c53ccf57d

This guide walks you through the process of sharding an existing unsharded
Vitess [keyspace](http://vitess.io/overview/concepts.html#keyspace) in Kubernetes.

**Contents:**
<div id="toc"></div>

## Prerequisites

We begin by assuming you've completed the
[Getting Started on Kubernetes](http://vitess.io/getting-started/) guide, and
have left the cluster running.

## Overview

We will follow a process similar to the one in the general
[Horizontal Sharding](http://vitess.io/user-guide/horizontal-sharding.html)
guide, except that here we'll give the exact commands you'll need to do it for
the example Vitess cluster in Kubernetes.

Since Vitess makes [sharding](http://vitess.io/user-guide/sharding.html)
transparent to the app layer, the
[Guestbook](https://github.com/youtube/vitess/tree/master/examples/kubernetes/guestbook)
sample app will stay live throughout the
[resharding](http://vitess.io/user-guide/sharding.html#resharding) process,
confirming that the Vitess cluster continues to serve without downtime.

## Configure sharding information

The first step is to tell Vitess the name and type of our
[keyspace_id](http://vitess.io/overview/concepts.html#keyspace-id) column:

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh SetKeyspaceShardingInfo test_keyspace keyspace_id uint64
```

This column was added in the original
[schema](https://github.com/youtube/vitess/blob/master/examples/kubernetes/create_test_table.sql)
for the unsharded example, although it was unnecessary there.
As a result, the Guestbook app is already sharding-ready,
so we don't need to change anything at the app layer to shard the database.

If the app hadn't originally been sharding-ready, we would need to alter
the tables to add the *keyspace_id* column, back-fill it with a suitable hash
of the sharding key, and include the *keyspace_id* in each query sent to VTGate.

See the [sharding guide](http://vitess.io/user-guide/sharding.html#range-based-sharding)
and the [Guestbook source](https://github.com/youtube/vitess/blob/master/examples/kubernetes/guestbook/main.py)
for more about how this column is used.

## Bring up tablets for new shards

In the unsharded example, you started tablets for a shard
named *0* in *test_keyspace*, written as *test_keyspace/0*.
Now you'll start tablets for two additional shards,
named *test_keyspace/-80* and *test_keyspace/80-*:

``` sh
vitess/examples/kubernetes$ ./sharded-vttablet-up.sh
### example output:
# Creating test_keyspace.shard--80 pods in cell test...
# ...
# Creating test_keyspace.shard-80- pods in cell test...
# ...
```

Since the sharding key in the Guestbook app is the page number,
this will result in half the pages going to each shard,
since *0x80* is the midpoint of the
[sharding key range](http://vitess.io/user-guide/sharding.html#key-ranges-and-partitions).

These new shards will run in parallel with the original shard during the
transition, but actual traffic will be served only by the original shard
until we tell it to switch over.

Check the `vtctld` **Topology** page, or the output of `kvtctl.sh ListAllTablets test`,
to see when the tablets are ready. There should be 5 tablets in each shard.

Once the tablets are ready, initialize replication by electing the first master
for each of the new shards:

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh InitShardMaster -force test_keyspace/-80 test-0000000200
vitess/examples/kubernetes$ ./kvtctl.sh InitShardMaster -force test_keyspace/80- test-0000000300
```

Now there should be a total of 15 tablets, with one master each:

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh ListAllTablets test
### example output:
# test-0000000100 test_keyspace 0 master 10.64.3.4:15002 10.64.3.4:3306 []
# ...
# test-0000000200 test_keyspace -80 master 10.64.0.7:15002 10.64.0.7:3306 []
# ...
# test-0000000300 test_keyspace 80- master 10.64.0.9:15002 10.64.0.9:3306 []
# ...
```

## Copy data from original shard

The new tablets start out empty, so we need to copy everything from the
original shard to the two new ones, starting with the schema:

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh CopySchemaShard test_keyspace/0 test_keyspace/-80
vitess/examples/kubernetes$ ./kvtctl.sh CopySchemaShard test_keyspace/0 test_keyspace/80-
```

Since the amount of data to copy can be very large, we use a special
batch process called `vtworker` to stream the data from a single source
to multiple destinations, routing each row based on its *keyspace_id*:

``` sh
vitess/examples/kubernetes$ ./sharded-vtworker.sh SplitClone --strategy=-populate_blp_checkpoint test_keyspace/0
### example output:
# Creating vtworker pod in cell test...
# pods/vtworker
# Following vtworker logs until termination...
# I0627 23:57:51.118768      10 vtworker.go:99] Starting worker...
# ...
# State: done
# Success:
# messages: copy done, copied 3 rows
# Deleting vtworker pod...
# pods/vtworker
```

Notice that we've only specified the source shard, *test_keyspace/0*.
The *SplitClone* process will automatically figure out which shards to use
as the destinations based on the key range that needs to be covered.
In this case, shard *0* covers the entire range, so it identifies
*-80* and *80-* as the destination shards, since they combine to cover the
same range.

Next, it will pause replication on one *rdonly* (offline processing) tablet
to serve as a consistent snapshot of the data. The app can continue without
downtime, since live traffic is served by *replica* and *master* tablets,
which are unaffected. Other batch jobs will also be unaffected, since they
will be served by the remaining, un-paused *rdonly* tablets.

## Check filtered replication

Once the copy from the paused snapshot finishes, `vtworker` turns on
[filtered replication](http://vitess.io/user-guide/sharding.html#filtered-replication)
from the source shard to each destination shard. This allows the destination
shards to catch up on updates that have continued to flow in from the app since
the time of the snapshot.

When the destination shards are caught up, they will continue to replicate
new updates. You can see this by looking at the contents of each shard as
you add new messages to various pages in the Guestbook app. Shard *0* will
see all the messages, while the new shards will only see messages for pages
that live on that shard.

``` sh
# See what's on shard test_keyspace/0:
vitess/examples/kubernetes$ ./kvtctl.sh ExecuteFetchAsDba test-0000000100 "SELECT * FROM messages"
# See what's on shard test_keyspace/-80:
vitess/examples/kubernetes$ ./kvtctl.sh ExecuteFetchAsDba test-0000000200 "SELECT * FROM messages"
# See what's on shard test_keyspace/80-:
vitess/examples/kubernetes$ ./kvtctl.sh ExecuteFetchAsDba test-0000000300 "SELECT * FROM messages"
```

## Check copied data integrity

The `vtworker` batch process has another mode that will compare the source
and destination to ensure all the data is present and correct.
The following commands will run a diff for each destination shard:

``` sh
vitess/examples/kubernetes$ ./sharded-vtworker.sh SplitDiff test_keyspace/-80
vitess/examples/kubernetes$ ./sharded-vtworker.sh SplitDiff test_keyspace/80-
```

If any discrepancies are found, they will be printed.
If everything is good, you should see something like this:

```
Table messages checks out (4 rows processed, 1072961 qps)
```

## Switch over to new shards

Now we're ready to switch over to serving from the new shards.
The [MigrateServedTypes](http://vitess.io/reference/vtctl.html#migrateservedtypes)
command lets you do this one
[tablet type](http://vitess.io/overview/concepts.html#tablet) at a time,
and even one [cell](http://vitess.io/overview/concepts.html#cell-(data-center))
at a time. The process can be rolled back at any point *until* the master is
switched over.

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh MigrateServedTypes test_keyspace/0 rdonly
vitess/examples/kubernetes$ ./kvtctl.sh MigrateServedTypes test_keyspace/0 replica
vitess/examples/kubernetes$ ./kvtctl.sh MigrateServedTypes test_keyspace/0 master
```

During the *master* migration, the original shard master will first stop
accepting updates. Then the process will wait for the new shard masters to
fully catch up on filtered replication before allowing them to begin serving.
Since filtered replication has been following along with live updates, there
should only be a few seconds of master unavailability.

## Remove original shard

Now that all traffic is being served from the new shards, we can remove the
original one. To do that, we use the `vttablet-down.sh` script from the
unsharded example:

``` sh
vitess/examples/kubernetes$ ./vttablet-down.sh
### example output:
# Removing tablet test-0000000100 from Vitess topology...
# Deleting pod for tablet test-0000000100...
# pods/vttablet-100
# ...
```

Then we can delete the now-empty shard:

``` sh
vitess/examples/kubernetes$ ./kvtctl.sh DeleteShard test_keyspace/0
```

You should then see in the vtctld **Topology** page, or in the output of
`kvtctl.sh ListAllTablets test` that the tablets for shard *0* are gone.

## Tear down and clean up

Before stopping the Container Engine cluster, you should tear down the Vitess
services. Kubernetes will then take care of cleaning up any entities it created
for those services, like external load balancers.

Since you already cleaned up the tablets from the original unsharded example by
running `./vttablet-down.sh`, that step has been replaced with
`./sharded-vttablet-down.sh` to clean up the new sharded tablets.

``` sh
vitess/examples/kubernetes$ ./guestbook-down.sh
vitess/examples/kubernetes$ ./vtgate-down.sh
vitess/examples/kubernetes$ ./sharded-vttablet-down.sh
vitess/examples/kubernetes$ ./vtctld-down.sh
vitess/examples/kubernetes$ ./etcd-down.sh
```

Then tear down the Container Engine cluster itself, which will stop the virtual
machines running on Compute Engine:

``` sh
$ gcloud beta container clusters delete example
```

It's also a good idea to remove the firewall rules you created, unless you plan
to use them again soon:

``` sh
$ gcloud compute firewall-rules delete vtctld guestbook
```

package launcher

import (
	"github.com/adamringhede/influxdb-ha/cluster"
	"github.com/adamringhede/influxdb-ha/service"
	"github.com/adamringhede/influxdb-ha/syncing"
	"github.com/coreos/etcd/clientv3"
	"log"
	"strings"
	"time"
)

const etcdTimeout = 5 * time.Second

// Start instantiates and links components used to run the cluster.
func Start(clusterID string, nodeName string, etcdEndpoints string, dataLocation string, httpConfig service.Config) {
	c, etcdErr := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(etcdEndpoints, ","),
		DialTimeout: etcdTimeout,
	})
	handleErr(etcdErr)

	// Setup storage components
	nodeStorage := cluster.NewEtcdNodeStorage(c)
	tokenStorage := cluster.NewEtcdTokenStorageWithClient(c)
	hintsStorage := cluster.NewEtcdHintStorage(c, nodeName)
	settingsStorage := cluster.NewEtcdSettingsStorage(c)
	partitionKeyStorage := cluster.NewEtcdPartitionKeyStorage(c)
	recoveryStorage := cluster.NewLocalRecoveryStorage("./", hintsStorage)
	authStorage := cluster.NewEtcdAuthStorage(c)

	nodeStorage.ClusterID = clusterID
	tokenStorage.ClusterID = clusterID
	hintsStorage.ClusterID = clusterID
	settingsStorage.ClusterID = clusterID
	partitionKeyStorage.ClusterID = clusterID
	authStorage.ClusterID = clusterID

	nodeCollection, err := cluster.NewSyncedNodeCollection(nodeStorage)
	handleErr(err)

	defaultReplicationFactor, err := settingsStorage.GetDefaultReplicationFactor(2)
	handleErr(err)

	resolver := cluster.NewResolverWithNodes(nodeCollection)
	_, err = cluster.NewResolverSyncer(resolver, tokenStorage, nodeCollection)
	handleErr(err)
	resolver.ReplicationFactor = defaultReplicationFactor

	partitioner, err := cluster.NewSyncedPartitioner(partitionKeyStorage)
	handleErr(err)

	localNode, isNew := initLocalNode(dataLocation, nodeName, nodeStorage)

	predicate := syncing.ClusterImportPredicate{
		LocalNode:     *localNode,
		PartitionKeys: partitioner,
		Resolver:      resolver,
	}

	partitioner.AddKey(cluster.PartitionKey{})
	importer := syncing.NewImporter(resolver, partitioner, predicate.Test)

	reliableImporter, importWQ := startImporter(importer, c, resolver, *localNode, clusterID)
	reliableImporter.AfterImport = func(token int) {
		tokenStorage.Assign(token, localNode.Name)
	}

	authService := service.NewPersistentAuthService(authStorage)

	// TODO change this to another way of handling node removal in the request handler.
	nodeStorage.OnRemove(NewClusterNodeDeallocator(nodeCollection, tokenStorage, resolver, hintsStorage, importWQ).Remove)

	go (func() {
		for rf := range settingsStorage.WatchDefaultReplicationFactor() {
			resolver.ReplicationFactor = rf
		}
	})()

	go cluster.RecoverNodes(hintsStorage, recoveryStorage, nodeCollection)
	go authService.Sync()

	// Starting the service here so that the node can receive writes while joining.
	go service.Start(resolver, partitioner, recoveryStorage, partitionKeyStorage, nodeStorage, authService, httpConfig)

	if isNew {
		err = Join(localNode, tokenStorage, nodeStorage, resolver, importer)
		handleErr(err)
	} else {
		recoverFailedWrites(hintsStorage, nodeStorage, localNode)
	}

	// Sleep forever
	select {}
}

type Runner struct {
	// various storage components that can mocked and possibly inspected
	// starts a bunch of services for syncing data
	// has various shared components like a resolver, authService, partitioner.
	// has a logger.
}

func initLocalNode(dataLocation string, nodeName string, nodeStorage cluster.NodeStorage) (*cluster.Node, bool) {
	localNode, nodeErr := nodeStorage.Get(nodeName)
	handleErr(nodeErr)

	isNew := localNode == nil || localNode.Status == cluster.NodeStatusJoining
	if localNode == nil {
		localNode = &cluster.Node{
			Status: cluster.NodeStatusJoining,
			Name:   nodeName,
		}
	}
	localNode.DataLocation = dataLocation
	handleErr(nodeStorage.Save(localNode))
	return localNode, isNew
}

func recoverFailedWrites(hintsStorage *cluster.EtcdHintStorage, nodeStorage cluster.NodeStorage, localNode *cluster.Node) {
	// Check if recovering (others nodes hold data)
	selfHints, err := hintsStorage.GetByTarget(localNode.Name)
	handleErr(err)
	if len(selfHints) != 0 {
		localNode.Status = cluster.NodeStatusRecovering
		nodeStorage.Save(localNode)
		<-cluster.WaitUntilRecovered(hintsStorage, localNode.Name)
	}
	localNode.Status = cluster.NodeStatusUp
	nodeStorage.Save(localNode)
}

func handleErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func startImporter(importer syncing.Importer, etcdClient *clientv3.Client, resolver *cluster.Resolver, localNode cluster.Node, clusterID string) (*syncing.ReliableImporter, cluster.WorkQueue) {
	wq := cluster.NewEtcdWorkQueue(etcdClient, localNode.Name, syncing.ReliableImportWorkName)
	wq.ClusterID = clusterID
	reliableImporter := syncing.NewReliableImporter(importer, wq, resolver, localNode.DataLocation)
	go reliableImporter.Start()
	return reliableImporter, wq
}

type NodeDeallocator interface {
	// Remove should reassign partitions from the node to other nodes in the cluster.
	Remove(cluster.Node)
}

type ClusterNodeDeallocator struct {
	nodeCollection cluster.NodeCollection
	tokenStorage   cluster.TokenStorage
	resolver       *cluster.Resolver
	hintsStorage   cluster.HintStorage
	importWQ       cluster.WorkQueue
}

func NewClusterNodeDeallocator(
	nodeCollection cluster.NodeCollection,
	tokenStorage cluster.TokenStorage,
	resolver *cluster.Resolver,
	hintsStorage cluster.HintStorage,
	importWQ cluster.WorkQueue,
) *ClusterNodeDeallocator {
	return &ClusterNodeDeallocator{nodeCollection, tokenStorage,
		resolver, hintsStorage, importWQ}
}

func (nd *ClusterNodeDeallocator) Remove(node cluster.Node) {
	// Distribute tokens to other nodes
	nodes := []string{}
	tokenGroups := map[string][]int{}
	for name := range nd.nodeCollection.GetAll() {
		if name != node.Name {
			if _, ok := tokenGroups[name]; !ok {
				nodes = append(nodes, name)
				tokenGroups[name] = []int{}
			}
		}
	}
	tokensMap, err := nd.tokenStorage.Get()
	if err != nil {
		// TODO Recover from being unable to get from tokenStorage
		return
	}
	var i int
	for token, nodeName := range tokensMap {
		if nodeName != node.Name {
			selectedNode := nodes[i%len(nodes)]
			tokenGroups[selectedNode] = append(tokenGroups[selectedNode], token)
			tokenGroups[selectedNode] = append(tokenGroups[selectedNode], nd.resolver.ReverseSecondaryLookup(token)...)
			i++
		}
	}
	for nodeName, tokens := range tokenGroups {
		nd.importWQ.Push(nodeName, syncing.ReliableImportPayload{Tokens: tokens, NonPartitioned: true})
	}
	// Remove all hints that may be held by this node. If the node is removed, there will be no way
	// for it to recover the data to the target node so we need to delete the hints so that the target
	// node will get the correct status and accept reads.
	hintsTargets, _ := nd.hintsStorage.GetByHolder()
	for _, target := range hintsTargets {
		nd.hintsStorage.Done(target)
	}
}

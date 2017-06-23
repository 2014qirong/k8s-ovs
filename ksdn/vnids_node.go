package ksdn

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/util/sets"
	utilwait "k8s.io/kubernetes/pkg/util/wait"

	. "k8s-ovs/pkg/etcdmanager"
	"k8s-ovs/pkg/vnid"
)

type nodeVNIDMap struct {
	// Synchronizes add or remove ids/namespaces
	lock       sync.Mutex
	ids        map[string]uint32
	namespaces map[uint32]sets.String
}

func newNodeVNIDMap() *nodeVNIDMap {
	return &nodeVNIDMap{
		ids:        make(map[string]uint32),
		namespaces: make(map[uint32]sets.String),
	}
}

func (vmap *nodeVNIDMap) addNamespaceToSet(name string, vnid uint32) {
	set, found := vmap.namespaces[vnid]
	if !found {
		set = sets.NewString()
		vmap.namespaces[vnid] = set
	}
	set.Insert(name)
}

func (vmap *nodeVNIDMap) removeNamespaceFromSet(name string, vnid uint32) {
	if set, found := vmap.namespaces[vnid]; found {
		set.Delete(name)
		if set.Len() == 0 {
			delete(vmap.namespaces, vnid)
		}
	}
}

func (vmap *nodeVNIDMap) GetNamespaces(id uint32) []string {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	if set, ok := vmap.namespaces[id]; ok {
		return set.List()
	} else {
		return nil
	}
}

func (vmap *nodeVNIDMap) GetVNID(name string) (uint32, error) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	if id, ok := vmap.ids[name]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
}

// Nodes asynchronously watch for both NetNamespaces and services
// NetNamespaces populates vnid map and services/pod-setup depend on vnid map
// If for some reason, vnid map propagation from master to node is slow
// and if service/pod-setup tries to lookup vnid map then it may fail.
// So, use this method to alleviate this problem. This method will
// retry vnid lookup before giving up.
func (vmap *nodeVNIDMap) WaitAndGetVNID(name string) (uint32, error) {
	var id uint32
	backoff := utilwait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   1.5,
		Steps:    5,
	}
	err := utilwait.ExponentialBackoff(backoff, func() (bool, error) {
		var err error
		id, err = vmap.GetVNID(name)
		return err == nil, nil
	})
	if err == nil {
		return id, nil
	} else {
		return 0, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
	}
}

func (vmap *nodeVNIDMap) setVNID(name string, id uint32) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	if oldId, found := vmap.ids[name]; found {
		vmap.removeNamespaceFromSet(name, oldId)
	}
	vmap.ids[name] = id
	vmap.addNamespaceToSet(name, id)

	glog.Infof("Associate netid %d to namespace %q", id, name)
}

func (vmap *nodeVNIDMap) unsetVNID(name string) (id uint32, err error) {
	vmap.lock.Lock()
	defer vmap.lock.Unlock()

	id, found := vmap.ids[name]
	if !found {
		return 0, fmt.Errorf("Failed to find netid for namespace: %s in vnid map", name)
	}
	vmap.removeNamespaceFromSet(name, id)
	delete(vmap.ids, name)
	glog.Infof("Dissociate netid %d from namespace %q", id, name)
	return id, nil
}

func (vmap *nodeVNIDMap) populateVNIDs(ctx context.Context, network string, eClient EtcdManager) error {
	netnsList, err := eClient.GetNetNamespaces(ctx, network)
	if err != nil {
		return err
	}

	glog.V(5).Infof("NetNamespaces %v, already exist!", netnsList)

	for _, net := range netnsList {
		vmap.setVNID(net.NetName, net.NetID)
	}

	return nil
}

//------------------ Node Methods --------------------

func (node *KsdnNode) VnidStartNode() error {
	// Populate vnid map synchronously so that existing services can fetch vnid
	err := node.vnids.populateVNIDs(node.ctx, node.networkInfo.name, node.eClient)
	if err != nil {
		return err
	}

	go utilwait.Forever(node.watchNetNamespaces, 0)
	go utilwait.Forever(node.watchServices, 0)
	return nil
}

func (node *KsdnNode) updatePodNetwork(namespace string, oldNetID, netID uint32) {
	// FIXME: this is racy; traffic coming from the pods gets switched to the new
	// VNID before the service and firewall rules are updated to match. We need
	// to do the updates as a single transaction (ovs-ofctl --bundle).

	runPods, otherPods, err := node.GetLocalPods(namespace)
	if err != nil {
		glog.Errorf("Could not get list of local pods in namespace %q: %v", namespace, err)
	}
	services, err := node.kClient.Services(namespace).List(kapi.ListOptions{})
	if err != nil {
		glog.Errorf("Could not get list of services in namespace %q: %v", namespace, err)
		services = &kapi.ServiceList{}
	}

	// Update OF rules for the existing/old pods in the namespace
	for _, pod := range runPods {
		err = node.UpdatePod(pod)
		if err != nil {
			glog.Errorf("Could not update pod %q in namespace %q: %v", pod.Name, namespace, err)
		}
	}

	deleteOptions := kapi.DeleteOptions{}
	for _, pod := range otherPods {
		err := node.kClient.Pods(namespace).Delete(pod.Name, &deleteOptions)
		if err != nil {
			glog.Errorf("Could not delete pod %q in namespace %q: %v", pod.Name, namespace, err)
		}
	}

	// Update OF rules for the old services in the namespace
	for _, svc := range services.Items {
		if !kapi.IsServiceIPSet(&svc) {
			continue
		}

		if err = node.DeleteServiceRules(&svc); err != nil {
			glog.Errorf("Error adding OVS flows for service %v, netid %d: %v", svc, netID, err)
		}
		if err = node.AddServiceRules(&svc, netID); err != nil {
			glog.Errorf("Error deleting OVS flows for service %v: %v", svc, err)
		}
	}
}

func (node *KsdnNode) nodeHandleNetnsEvent(batch []Event) {
	for _, evt := range batch {
		netns := evt.NetNS
		switch evt.Type {
		case EventAdded:
			oldNetID, err := node.vnids.GetVNID(netns.NetName)
			if (err == nil) && (oldNetID == netns.NetID) {
				break
			}
			node.vnids.setVNID(netns.NetName, netns.NetID)
			node.updatePodNetwork(netns.NetName, oldNetID, netns.NetID)
		case EventRemoved:
			// updatePodNetwork needs vnid, so unset vnid after this call
			node.updatePodNetwork(netns.NetName, netns.NetID, vnid.GlobalVNID)
			node.vnids.unsetVNID(netns.NetName)

		default:
			glog.Error("Internal error: unknown event type: ", int(evt.Type))
		}
	}
}

func (node *KsdnNode) watchNetNamespaces() {
	receiver := make(chan []Event)
	RunNetnsWatch(node.ctx, node.eClient, node.networkInfo.name, receiver, node.nodeHandleNetnsEvent)
}

func isServiceChanged(oldsvc, newsvc *kapi.Service) bool {
	if len(oldsvc.Spec.Ports) == len(newsvc.Spec.Ports) {
		for i := range oldsvc.Spec.Ports {
			if oldsvc.Spec.Ports[i].Protocol != newsvc.Spec.Ports[i].Protocol ||
				oldsvc.Spec.Ports[i].Port != newsvc.Spec.Ports[i].Port {
				return true
			}
		}
		return false
	}
	return true
}

func (node *KsdnNode) watchServices() {
	services := make(map[string]*kapi.Service)
	RunEventQueue(node.kClient, Services, func(delta cache.Delta) error {
		serv := delta.Object.(*kapi.Service)

		// Ignore headless services
		if !kapi.IsServiceIPSet(serv) {
			return nil
		}

		glog.V(5).Infof("Watch %s event for Service %q", delta.Type, serv.ObjectMeta.Name)
		switch delta.Type {
		case cache.Sync, cache.Added, cache.Updated:
			oldsvc, exists := services[string(serv.UID)]
			if exists {
				if !isServiceChanged(oldsvc, serv) {
					break
				}
				if err := node.DeleteServiceRules(oldsvc); err != nil {
					glog.Error(err)
				}
			}

			netid, err := node.vnids.WaitAndGetVNID(serv.Namespace)
			if err != nil {
				return fmt.Errorf("skipped adding service rules for serviceEvent: %v, Error: %v", delta.Type, err)
			}

			if err = node.AddServiceRules(serv, netid); err != nil {
				return err
			}
			services[string(serv.UID)] = serv
		case cache.Deleted:
			delete(services, string(serv.UID))
			if err := node.DeleteServiceRules(serv); err != nil {
				return err
			}
		}
		return nil
	})
}

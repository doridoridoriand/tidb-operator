// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/gomega"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb-operator/pkg/apis/label"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/apis/util/toml"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/manager/suspender"
	"github.com/pingcap/tidb-operator/pkg/manager/volumes"
	"github.com/pingcap/tidb-operator/pkg/pdapi"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

func TestTiKVMemberManagerSyncCreate(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name                         string
		prepare                      func(cluster *v1alpha1.TidbCluster)
		errWhenCreateStatefulSet     bool
		errWhenCreateTiKVPeerService bool
		errWhenGetStores             bool
		suspendComponent             func() (bool, error)
		err                          bool
		tls                          bool
		tikvPeerSvcCreated           bool
		setCreated                   bool
		pdStores                     *pdapi.StoresInfo
		tombstoneStores              *pdapi.StoresInfo
	}

	testFn := func(test *testcase, t *testing.T) {
		t.Log(test.name)

		tc := newTidbClusterForPD()
		if test.tls {
			tc.Spec.TLSCluster = &v1alpha1.TLSCluster{Enabled: true}
		}
		tc.Status.PD.Members = map[string]v1alpha1.PDMember{
			"pd-0": {Name: "pd-0", Health: true},
			"pd-1": {Name: "pd-1", Health: true},
			"pd-2": {Name: "pd-2", Health: true},
		}
		tc.Status.PD.StatefulSet = &apps.StatefulSetStatus{ReadyReplicas: 3}

		ns := tc.Namespace
		tcName := tc.Name
		oldSpec := tc.Spec
		if test.prepare != nil {
			test.prepare(tc)
		}

		tkmm, fakeSetControl, fakeSvcControl, pdClient, _, _ := newFakeTiKVMemberManager(tc)
		pdClient.AddReaction(pdapi.GetConfigActionType, func(action *pdapi.Action) (interface{}, error) {
			return &v1alpha1.PDConfig{
				Replication: &v1alpha1.PDReplicationConfig{
					LocationLabels: []string{"region", "zone", "rack", "host"},
				},
			}, nil
		})
		if test.errWhenGetStores {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("failed to get stores from tikv cluster")
			})
		} else {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.pdStores, nil
			})
			pdClient.AddReaction(pdapi.GetTombStoneStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.tombstoneStores, nil
			})
			pdClient.AddReaction(pdapi.SetStoreLabelsActionType, func(action *pdapi.Action) (interface{}, error) {
				return true, nil
			})
		}

		if test.errWhenCreateStatefulSet {
			fakeSetControl.SetCreateStatefulSetError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}
		if test.errWhenCreateTiKVPeerService {
			fakeSvcControl.SetCreateServiceError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}
		if test.suspendComponent != nil {
			tkmm.suspender.(*suspender.FakeSuspender).SuspendComponentFunc = func(c v1alpha1.Cluster, mt v1alpha1.MemberType) (bool, error) {
				return test.suspendComponent()
			}
		}

		err := tkmm.Sync(tc)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}

		g.Expect(tc.Spec).To(Equal(oldSpec))

		svc, err := tkmm.deps.ServiceLister.Services(ns).Get(controller.TiKVPeerMemberName(tcName))
		if test.tikvPeerSvcCreated {
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(svc).NotTo(Equal(nil))
		} else {
			expectErrIsNotFound(g, err)
		}

		tc1, err := tkmm.deps.StatefulSetLister.StatefulSets(ns).Get(controller.TiKVMemberName(tcName))
		if test.setCreated {
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tc1).NotTo(Equal(nil))
		} else {
			expectErrIsNotFound(g, err)
		}
	}

	tests := []testcase{
		{
			name:                         "normal",
			prepare:                      nil,
			errWhenCreateStatefulSet:     false,
			errWhenCreateTiKVPeerService: false,
			err:                          false,
			tikvPeerSvcCreated:           true,
			setCreated:                   true,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
		{
			name:                         "normal with tls",
			prepare:                      nil,
			errWhenCreateStatefulSet:     false,
			errWhenCreateTiKVPeerService: false,
			err:                          false,
			tls:                          true,
			tikvPeerSvcCreated:           true,
			setCreated:                   true,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
		{
			name: "pd is not available",
			prepare: func(tc *v1alpha1.TidbCluster) {
				tc.Status.PD.Members = map[string]v1alpha1.PDMember{}
			},
			errWhenCreateStatefulSet:     false,
			errWhenCreateTiKVPeerService: false,
			err:                          true,
			tikvPeerSvcCreated:           false,
			setCreated:                   false,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
		{
			name:                         "error when create statefulset",
			prepare:                      nil,
			errWhenCreateStatefulSet:     true,
			errWhenCreateTiKVPeerService: false,
			err:                          true,
			tikvPeerSvcCreated:           true,
			setCreated:                   false,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
		{
			name:                         "error when create tikv peer service",
			prepare:                      nil,
			errWhenCreateStatefulSet:     false,
			errWhenCreateTiKVPeerService: true,
			err:                          true,
			tikvPeerSvcCreated:           false,
			setCreated:                   false,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
		{
			name:                         "skip create when suspend",
			suspendComponent:             func() (bool, error) { return true, nil },
			prepare:                      nil,
			errWhenCreateStatefulSet:     true,
			errWhenCreateTiKVPeerService: true,
			err:                          false,
			tikvPeerSvcCreated:           false,
			setCreated:                   false,
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
		},
	}

	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestTiKVMemberManagerSyncUpdate(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name                         string
		modify                       func(cluster *v1alpha1.TidbCluster)
		pdStores                     *pdapi.StoresInfo
		tombstoneStores              *pdapi.StoresInfo
		errWhenUpdateStatefulSet     bool
		errWhenUpdateTiKVPeerService bool
		errWhenGetStores             bool
		statusChange                 func(*apps.StatefulSet)
		err                          bool
		expectTiKVPeerServiceFn      func(*GomegaWithT, *corev1.Service, error)
		expectStatefulSetFn          func(*GomegaWithT, *apps.StatefulSet, error)
		expectTidbClusterFn          func(*GomegaWithT, *v1alpha1.TidbCluster)
	}

	testFn := func(test *testcase, t *testing.T) {
		t.Log(test.name)

		tc := newTidbClusterForPD()
		tc.Status.PD.Members = map[string]v1alpha1.PDMember{
			"pd-0": {Name: "pd-0", Health: true},
			"pd-1": {Name: "pd-1", Health: true},
			"pd-2": {Name: "pd-2", Health: true},
		}
		tc.Status.PD.StatefulSet = &apps.StatefulSetStatus{ReadyReplicas: 3}

		ns := tc.Namespace
		tcName := tc.Name

		tkmm, fakeSetControl, fakeSvcControl, pdClient, _, _ := newFakeTiKVMemberManager(tc)
		pdClient.AddReaction(pdapi.GetConfigActionType, func(action *pdapi.Action) (interface{}, error) {
			return &pdapi.PDConfigFromAPI{
				Replication: &pdapi.PDReplicationConfig{
					LocationLabels: []string{"region", "zone", "rack", "host"},
				},
			}, nil
		})
		if test.errWhenGetStores {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("failed to get stores from pd cluster")
			})
		} else {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.pdStores, nil
			})
			pdClient.AddReaction(pdapi.GetTombStoneStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.tombstoneStores, nil
			})
			pdClient.AddReaction(pdapi.SetStoreLabelsActionType, func(action *pdapi.Action) (interface{}, error) {
				return true, nil
			})
		}

		if test.statusChange == nil {
			fakeSetControl.SetStatusChange(func(set *apps.StatefulSet) {
				set.Status.Replicas = *set.Spec.Replicas
				set.Status.CurrentRevision = "pd-1"
				set.Status.UpdateRevision = "pd-1"
				observedGeneration := int64(1)
				set.Status.ObservedGeneration = observedGeneration
			})
		} else {
			fakeSetControl.SetStatusChange(test.statusChange)
		}

		err := tkmm.Sync(tc)
		g.Expect(err).NotTo(HaveOccurred())

		_, err = tkmm.deps.ServiceLister.Services(ns).Get(controller.TiKVPeerMemberName(tcName))
		g.Expect(err).NotTo(HaveOccurred())
		_, err = tkmm.deps.StatefulSetLister.StatefulSets(ns).Get(controller.TiKVMemberName(tcName))
		g.Expect(err).NotTo(HaveOccurred())

		tc1 := tc.DeepCopy()
		test.modify(tc1)

		if test.errWhenUpdateTiKVPeerService {
			fakeSvcControl.SetUpdateServiceError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}
		if test.errWhenUpdateStatefulSet {
			fakeSetControl.SetUpdateStatefulSetError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}

		err = tkmm.Sync(tc1)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}

		if test.expectTiKVPeerServiceFn != nil {
			svc, err := tkmm.deps.ServiceLister.Services(ns).Get(controller.TiKVPeerMemberName(tcName))
			test.expectTiKVPeerServiceFn(g, svc, err)
		}
		if test.expectStatefulSetFn != nil {
			set, err := tkmm.deps.StatefulSetLister.StatefulSets(ns).Get(controller.TiKVMemberName(tcName))
			test.expectStatefulSetFn(g, set, err)
		}
		if test.expectTidbClusterFn != nil {
			test.expectTidbClusterFn(g, tc1)
		}
	}

	tests := []testcase{
		{
			name: "normal",
			modify: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.Replicas = 5
				tc.Spec.Services = []v1alpha1.Service{
					{Name: "tikv", Type: string(corev1.ServiceTypeNodePort)},
				}
				tc.Status.PD.Phase = v1alpha1.NormalPhase
			},
			// TODO add unit test for status sync
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			errWhenUpdateStatefulSet:     false,
			errWhenUpdateTiKVPeerService: false,
			errWhenGetStores:             false,
			err:                          false,
			expectTiKVPeerServiceFn:      nil,
			expectStatefulSetFn: func(g *GomegaWithT, set *apps.StatefulSet, err error) {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(int(*set.Spec.Replicas)).To(Equal(4))
			},
			expectTidbClusterFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.ObservedGeneration).To(Equal(int64(1)))
				g.Expect(tc.Status.TiKV.Stores).To(Equal(map[string]v1alpha1.TiKVStore{}))
				g.Expect(tc.Status.TiKV.TombstoneStores).To(Equal(map[string]v1alpha1.TiKVStore{}))
			},
		},
		{
			name: "error when update statefulset",
			modify: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.Replicas = 5
				tc.Status.PD.Phase = v1alpha1.NormalPhase
			},
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			errWhenUpdateStatefulSet:     true,
			errWhenUpdateTiKVPeerService: false,
			err:                          true,
			expectTiKVPeerServiceFn:      nil,
			expectStatefulSetFn: func(g *GomegaWithT, set *apps.StatefulSet, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
		},
		{
			name: "error when sync tikv status",
			modify: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.Replicas = 5
				tc.Status.PD.Phase = v1alpha1.NormalPhase
			},
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			errWhenUpdateStatefulSet:     false,
			errWhenUpdateTiKVPeerService: false,
			errWhenGetStores:             true,
			err:                          true,
			expectTiKVPeerServiceFn:      nil,
			expectStatefulSetFn: func(g *GomegaWithT, set *apps.StatefulSet, err error) {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(int(*set.Spec.Replicas)).To(Equal(3))
			},
			expectTidbClusterFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(0))
			},
		},
		{
			name: "enable separate RocksDB and Raft log on the fly",
			modify: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.SeparateRocksDBLog = pointer.BoolPtr(true)
				tc.Spec.TiKV.SeparateRaftLog = pointer.BoolPtr(true)
			},
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			errWhenUpdateStatefulSet:     false,
			errWhenUpdateTiKVPeerService: false,
			errWhenGetStores:             false,
			err:                          false,
			expectTiKVPeerServiceFn:      nil,
			expectStatefulSetFn: func(g *GomegaWithT, set *apps.StatefulSet, err error) {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(set.Spec.Template.Spec.Containers).To(HaveLen(3))
			},
			expectTidbClusterFn: nil,
		},
		{
			name: "template updates blocked on volume replace",
			modify: func(tc *v1alpha1.TidbCluster) {
				// Random test change to affect pod spec template.
				tc.Spec.TiKV.ServiceAccount = "test_new_account"
				tc.Spec.TiKV.SpareVolReplaceReplicas = pointer.Int32(1)
				tc.Status.TiKV.VolReplaceInProgress = true
			},
			pdStores:                     &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			tombstoneStores:              &pdapi.StoresInfo{Count: 0, Stores: []*pdapi.StoreInfo{}},
			errWhenUpdateStatefulSet:     false,
			errWhenUpdateTiKVPeerService: false,
			errWhenGetStores:             false,
			err:                          false,
			expectTiKVPeerServiceFn:      nil,
			expectStatefulSetFn: func(g *GomegaWithT, set *apps.StatefulSet, err error) {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(set.Spec.Template.Spec.ServiceAccountName).To(Equal(""))
			},
			expectTidbClusterFn: nil,
		},
	}

	for i := range tests {
		t.Logf("begin: %s", tests[i].name)
		testFn(&tests[i], t)
		t.Logf("end: %s", tests[i].name)
	}
}

func TestTiKVMemberManagerTiKVStatefulSetIsUpgrading(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name            string
		setUpdate       func(*apps.StatefulSet)
		hasPod          bool
		updatePod       func(*corev1.Pod)
		errExpectFn     func(*GomegaWithT, error)
		expectUpgrading bool
	}
	testFn := func(test *testcase, t *testing.T) {
		tc := newTidbClusterForPD()
		pmm, _, _, _, podIndexer, _ := newFakeTiKVMemberManager(tc)
		tc.Status.TiKV.StatefulSet = &apps.StatefulSetStatus{
			UpdateRevision: "v3",
		}

		set := &apps.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test",
				Namespace: metav1.NamespaceDefault,
			},
		}
		if test.setUpdate != nil {
			test.setUpdate(set)
		}

		if test.hasPod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        ordinalPodName(v1alpha1.TiKVMemberType, tc.GetName(), 0),
					Namespace:   metav1.NamespaceDefault,
					Annotations: map[string]string{},
					Labels:      label.New().Instance(tc.GetInstanceName()).TiKV().Labels(),
				},
			}
			if test.updatePod != nil {
				test.updatePod(pod)
			}
			podIndexer.Add(pod)
		}
		b, err := pmm.statefulSetIsUpgradingFn(pmm.deps.PodLister, pmm.deps.PDControl, set, tc)
		if test.errExpectFn != nil {
			test.errExpectFn(g, err)
		}
		if test.expectUpgrading {
			g.Expect(b).To(BeTrue())
		} else {
			g.Expect(b).NotTo(BeTrue())
		}
	}
	tests := []testcase{
		{
			name: "stateful set is upgrading",
			setUpdate: func(set *apps.StatefulSet) {
				set.Status.CurrentRevision = "v1"
				set.Status.UpdateRevision = "v2"
				set.Status.ObservedGeneration = 1000
			},
			hasPod:          false,
			updatePod:       nil,
			errExpectFn:     errExpectNil,
			expectUpgrading: true,
		},
		{
			name:            "pod don't have revision hash",
			setUpdate:       nil,
			hasPod:          true,
			updatePod:       nil,
			errExpectFn:     errExpectNil,
			expectUpgrading: false,
		},
		{
			name:      "pod have revision hash, not equal statefulset's",
			setUpdate: nil,
			hasPod:    true,
			updatePod: func(pod *corev1.Pod) {
				pod.Labels[apps.ControllerRevisionHashLabelKey] = "v2"
			},
			errExpectFn:     errExpectNil,
			expectUpgrading: true,
		},
		{
			name:      "pod have revision hash, equal statefulset's",
			setUpdate: nil,
			hasPod:    true,
			updatePod: func(pod *corev1.Pod) {
				pod.Labels[apps.ControllerRevisionHashLabelKey] = "v3"
			},
			errExpectFn:     errExpectNil,
			expectUpgrading: false,
		},
	}

	for i := range tests {
		t.Logf(tests[i].name)
		testFn(&tests[i], t)
	}
}

func TestTiKVMemberManagerSetStoreLabelsForTiKV(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name             string
		errWhenGetStores bool
		hasNode          bool
		hasPod           bool
		storeInfo        *pdapi.StoresInfo
		errExpectFn      func(*GomegaWithT, error)
		setCount         int
		labelSetFailed   bool
	}
	testFn := func(test *testcase, t *testing.T) {
		tc := newTidbClusterForPD()
		tc.Status.TiKV.BootStrapped = true
		pmm, _, _, pdClient, podIndexer, nodeIndexer := newFakeTiKVMemberManager(tc)
		pdClient.AddReaction(pdapi.GetConfigActionType, func(action *pdapi.Action) (interface{}, error) {
			return &pdapi.PDConfigFromAPI{
				Replication: &pdapi.PDReplicationConfig{
					LocationLabels: []string{"region", "zone", "rack", "host"},
				},
			}, nil
		})
		if test.errWhenGetStores {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("failed to get stores")
			})
		}
		if test.storeInfo != nil {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.storeInfo, nil
			})
		}
		if test.hasNode {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-1",
					Labels: map[string]string{
						"region":             "region",
						"zone":               "zone",
						"rack":               "rack",
						corev1.LabelHostname: "host",
					},
				},
			}
			nodeIndexer.Add(node)
		}
		if test.hasPod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-tikv-1",
					Namespace: metav1.NamespaceDefault,
				},
				Spec: corev1.PodSpec{
					NodeName: "node-1",
				},
			}
			podIndexer.Add(pod)
		}
		if test.labelSetFailed {
			pdClient.AddReaction(pdapi.SetStoreLabelsActionType, func(action *pdapi.Action) (interface{}, error) {
				return false, fmt.Errorf("label set failed")
			})
		} else {
			pdClient.AddReaction(pdapi.SetStoreLabelsActionType, func(action *pdapi.Action) (interface{}, error) {
				return true, nil
			})
		}

		setCount, err := pmm.setStoreLabelsForTiKV(tc)
		if test.errExpectFn != nil {
			test.errExpectFn(g, err)
		}
		g.Expect(setCount).To(Equal(test.setCount))
	}
	tests := []testcase{
		{
			name:             "get stores return error",
			errWhenGetStores: true,
			storeInfo:        nil,
			hasNode:          true,
			hasPod:           true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).To(HaveOccurred())
				g.Expect(strings.Contains(err.Error(), "failed to get stores")).To(BeTrue())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "stores is empty",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "status is nil",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Status: nil,
					},
				},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "store is nil",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: nil,
					},
				},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "don't have pod",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LeaderCount:     1,
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			hasNode: true,
			hasPod:  false,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).To(HaveOccurred())
				g.Expect(strings.Contains(err.Error(), "not found")).To(BeTrue())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "don't have node",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LeaderCount:     1,
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			hasNode: false,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "already has labels",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
								Labels: []*metapb.StoreLabel{
									{
										Key:   "region",
										Value: "region",
									},
									{
										Key:   "zone",
										Value: "zone",
									},
									{
										Key:   "rack",
										Value: "rack",
									},
									{
										Key:   "host",
										Value: "host",
									},
								},
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LeaderCount:     1,
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: false,
		},
		{
			name:             "labels not equal, but set failed",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
								Labels: []*metapb.StoreLabel{
									{
										Key:   "region",
										Value: "region",
									},
								},
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LeaderCount:     1,
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       0,
			labelSetFailed: true,
		},
		{
			name:             "labels not equal, set success",
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
								Labels: []*metapb.StoreLabel{
									{
										Key:   "region",
										Value: "region",
									},
								},
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LeaderCount:     1,
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			hasNode: true,
			hasPod:  true,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).NotTo(HaveOccurred())
			},
			setCount:       1,
			labelSetFailed: false,
		},
	}

	for i := range tests {
		t.Logf(tests[i].name)
		testFn(&tests[i], t)
	}
}

func TestTiKVMemberManagerSyncTidbClusterStatus(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name                      string
		updateTC                  func(*v1alpha1.TidbCluster)
		upgradingFn               func(corelisters.PodLister, pdapi.PDControlInterface, *apps.StatefulSet, *v1alpha1.TidbCluster) (bool, error)
		errWhenGetStores          bool
		storeInfo                 *pdapi.StoresInfo
		errWhenGetTombstoneStores bool
		tombstoneStoreInfo        *pdapi.StoresInfo
		errExpectFn               func(*GomegaWithT, error)
		tcExpectFn                func(*GomegaWithT, *v1alpha1.TidbCluster)
	}
	status := apps.StatefulSetStatus{
		Replicas: int32(3),
	}
	spec := apps.StatefulSetSpec{
		Replicas: pointer.Int32Ptr(3),
	}
	now := metav1.Time{Time: time.Now()}

	testFn := func(test *testcase, t *testing.T) {
		tc := newTidbClusterForPD()
		tc.Status.PD.Phase = v1alpha1.NormalPhase
		set := &apps.StatefulSet{
			Spec:   spec,
			Status: status,
		}
		if test.updateTC != nil {
			test.updateTC(tc)
		}
		pmm, _, _, pdClient, _, _ := newFakeTiKVMemberManager(tc)

		if test.upgradingFn != nil {
			pmm.statefulSetIsUpgradingFn = test.upgradingFn
		}
		if test.errWhenGetStores {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("failed to get stores")
			})
		} else if test.storeInfo != nil {
			pdClient.AddReaction(pdapi.GetStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.storeInfo, nil
			})
		}
		if test.errWhenGetTombstoneStores {
			pdClient.AddReaction(pdapi.GetTombStoneStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("failed to get tombstone stores")
			})
		} else if test.tombstoneStoreInfo != nil {
			pdClient.AddReaction(pdapi.GetTombStoneStoresActionType, func(action *pdapi.Action) (interface{}, error) {
				return test.tombstoneStoreInfo, nil
			})
		}

		err := pmm.syncTiKVClusterStatus(tc, set)
		if test.errExpectFn != nil {
			test.errExpectFn(g, err)
		}
		if test.tcExpectFn != nil {
			test.tcExpectFn(g, tc)
		}
	}

	tests := []testcase{
		{
			name:     "whether statefulset is upgrading returns failed",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, fmt.Errorf("whether upgrading failed")
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).To(HaveOccurred())
				g.Expect(strings.Contains(err.Error(), "whether upgrading failed")).To(BeTrue())
			},
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
			},
		},
		{
			name:     "statefulset is upgrading",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return true, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.UpgradePhase))
			},
		},
		{
			name: "statefulset is upgrading but pd is upgrading",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.PD.Phase = v1alpha1.UpgradePhase
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return true, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.NormalPhase))
			},
		},
		{
			name:     "statefulset is not upgrading",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.NormalPhase))
			},
		},
		{
			name: "statefulset is upgrading but someone is evicting leader",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Phase = v1alpha1.NormalPhase
				tc.Status.TiKV.SetCondition(metav1.Condition{
					Type:   v1alpha1.ConditionTypeLeaderEvicting,
					Status: metav1.ConditionTrue,
				})
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return true, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.NormalPhase))
			},
		},
		{
			name: "statefulset is scaling out",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.Replicas = 4
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.ScalePhase))
			},
		},
		{
			name: "statefulset is scaling in",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.TiKV.Replicas = 2
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores:          false,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn:               nil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Phase).To(Equal(v1alpha1.ScalePhase))
			},
		},
		{
			name:     "get stores failed",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores:          true,
			storeInfo:                 nil,
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo:        nil,
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).To(HaveOccurred())
				g.Expect(strings.Contains(err.Error(), "failed to get stores")).To(BeTrue())
			},
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(tc.Status.TiKV.StatefulSet.Replicas).To(Equal(int32(3)))
				g.Expect(tc.Status.TiKV.Synced).To(BeFalse())
			},
		},
		{
			name:     "stores is empty",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(0))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name:     "store is nil",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: nil,
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(0))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name:     "status is nil",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Status: nil,
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(0))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "LastHeartbeatTS is zero, TidbClulster LastHeartbeatTS is not zero",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{}
			},

			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(time.Time{}.IsZero()).To(BeTrue())
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "LastHeartbeatTS is zero, TidbClulster LastHeartbeatTS is zero",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(time.Time{}.IsZero()).To(BeTrue())
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "LastHeartbeatTS is not zero, TidbClulster LastHeartbeatTS is zero",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(time.Time{}.IsZero()).To(BeTrue())
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "set LastTransitionTime first time",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				// tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(tc.Status.TiKV.Stores["333"].LastTransitionTime.Time.IsZero()).To(BeFalse())
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "state not change, LastTransitionTime not change",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{
					LastTransitionTime: now,
					State:              v1alpha1.TiKVStateUp,
				}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(tc.Status.TiKV.Stores["333"].LastTransitionTime).To(Equal(now))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "state change, LastTransitionTime change",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{
					LastTransitionTime: now,
					State:              v1alpha1.TiKVStateUp,
				}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Down",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(tc.Status.TiKV.Stores["333"].LastTransitionTime).NotTo(Equal(now))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "get tombstone stores failed",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Status.TiKV.Stores = map[string]v1alpha1.TiKVStore{}
				tc.Status.TiKV.Stores["333"] = v1alpha1.TiKVStore{
					LastTransitionTime: now,
					State:              v1alpha1.TiKVStateUp,
				}
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Time{},
						},
					},
				},
			},
			errWhenGetTombstoneStores: true,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: func(g *GomegaWithT, err error) {
				g.Expect(err).To(HaveOccurred())
				g.Expect(strings.Contains(err.Error(), "failed to get tombstone stores")).To(BeTrue())
			},
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(0))
				g.Expect(tc.Status.TiKV.Synced).To(BeFalse())
			},
		},
		{
			name:     "all works",
			updateTC: nil,
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Tombstone",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc:20160", "test", "test", "default"),
							},
							StateName: "Tombstone",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.TombstoneStores)).To(Equal(1))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
		{
			name: "get TiKV Store and PeerStores without TiFlash Stores",
			updateTC: func(tc *v1alpha1.TidbCluster) {
				tc.Spec.ClusterDomain = "cluster1.com"
			},
			upgradingFn: func(lister corelisters.PodLister, controlInterface pdapi.PDControlInterface, set *apps.StatefulSet, cluster *v1alpha1.TidbCluster) (bool, error) {
				return false, nil
			},
			errWhenGetStores: false,
			storeInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      333,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc.cluster1.com:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      334,
								Address: fmt.Sprintf("%s-tikv-1.%s-tikv-peer.%s.svc.cluster2.com:20160", "test", "test", "default"),
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
					{
						Store: &pdapi.MetaStore{
							Store: &metapb.Store{
								Id:      330,
								Address: fmt.Sprintf("%s-tiflash-1.%s-tiflash-peer.%s.svc.cluster1.com:20160", "test", "test", "default"),
								Labels: []*metapb.StoreLabel{
									{
										Key:   "engine",
										Value: "tiflash",
									},
								},
							},
							StateName: "Up",
						},
						Status: &pdapi.StoreStatus{
							LastHeartbeatTS: time.Now(),
						},
					},
				},
			},
			errWhenGetTombstoneStores: false,
			tombstoneStoreInfo: &pdapi.StoresInfo{
				Stores: []*pdapi.StoreInfo{},
			},
			errExpectFn: errExpectNil,
			tcExpectFn: func(g *GomegaWithT, tc *v1alpha1.TidbCluster) {
				g.Expect(len(tc.Status.TiKV.Stores)).To(Equal(1))
				g.Expect(len(tc.Status.TiKV.PeerStores)).To(Equal(1))
				g.Expect(tc.Status.TiKV.Synced).To(BeTrue())
			},
		},
	}

	for i := range tests {
		t.Logf(tests[i].name)
		testFn(&tests[i], t)
	}
}

func newFakeTiKVMemberManager(tc *v1alpha1.TidbCluster) (
	*tikvMemberManager, *controller.FakeStatefulSetControl,
	*controller.FakeServiceControl, *pdapi.FakePDClient, cache.Indexer, cache.Indexer) {
	fakeDeps := controller.NewFakeDependencies()
	tmm := &tikvMemberManager{
		deps:                     fakeDeps,
		scaler:                   NewFakeTiKVScaler(),
		upgrader:                 NewFakeTiKVUpgrader(),
		statefulSetIsUpgradingFn: tikvStatefulSetIsUpgrading,
		suspender:                suspender.NewFakeSuspender(),
		podVolumeModifier:        &volumes.FakePodVolumeModifier{},
	}
	setControl := fakeDeps.StatefulSetControl.(*controller.FakeStatefulSetControl)
	svcControl := fakeDeps.ServiceControl.(*controller.FakeServiceControl)
	pdControl := fakeDeps.PDControl.(*pdapi.FakePDControl)
	pdClient := controller.NewFakePDClient(pdControl, tc)
	podIndexer := fakeDeps.KubeInformerFactory.Core().V1().Pods().Informer().GetIndexer()
	nodeIndexer := fakeDeps.KubeInformerFactory.Core().V1().Nodes().Informer().GetIndexer()
	return tmm, setControl, svcControl, pdClient, podIndexer, nodeIndexer
}

func TestGetNewTiKVServiceForTidbCluster(t *testing.T) {
	tests := []struct {
		name      string
		tc        v1alpha1.TidbCluster
		svcConfig SvcConfig
		expected  corev1.Service
	}{
		{
			name: "basic",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "ns",
				},
			},
			svcConfig: SvcConfig{
				Name:       "peer",
				Port:       v1alpha1.DefaultTiKVServerPort,
				Headless:   true,
				SvcLabel:   func(l label.Label) label.Label { return l.TiKV() },
				MemberName: controller.TiKVPeerMemberName,
			},
			expected: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo-tikv-peer",
					Namespace: "ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       "tidb-cluster",
						"app.kubernetes.io/managed-by": "tidb-operator",
						"app.kubernetes.io/instance":   "foo",
						"app.kubernetes.io/component":  "tikv",
						"app.kubernetes.io/used-by":    "peer",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "pingcap.com/v1alpha1",
							Kind:       "TidbCluster",
							Name:       "foo",
							UID:        "",
							Controller: func(b bool) *bool {
								return &b
							}(true),
							BlockOwnerDeletion: func(b bool) *bool {
								return &b
							}(true),
						},
					},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "None",
					Ports: []corev1.ServicePort{
						{
							Name:       "peer",
							Port:       v1alpha1.DefaultTiKVServerPort,
							TargetPort: intstr.FromInt(int(v1alpha1.DefaultTiKVServerPort)),
							Protocol:   corev1.ProtocolTCP,
						},
					},
					Selector: map[string]string{
						"app.kubernetes.io/name":       "tidb-cluster",
						"app.kubernetes.io/managed-by": "tidb-operator",
						"app.kubernetes.io/instance":   "foo",
						"app.kubernetes.io/component":  "tikv",
					},
					PublishNotReadyAddresses: true,
				},
			},
		},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			svc := getNewServiceForTidbCluster(&tt.tc, tt.svcConfig)
			if diff := cmp.Diff(tt.expected, *svc); diff != "" {
				t.Errorf("unexpected plugin configuration (-want, +got): %s", diff)
			}
		})
	}
}

func TestGetNewTiKVSetForTidbCluster(t *testing.T) {
	enable := true
	tests := []struct {
		name    string
		tc      v1alpha1.TidbCluster
		wantErr bool
		testSts func(sts *apps.StatefulSet)
	}{
		{
			name: "tikv network is not host",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testHostNetwork(t, false, ""),
		},
		{
			name: "tikv network is host",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							HostNetwork: &enable,
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testHostNetwork(t, true, corev1.DNSClusterFirstWithHostNet),
		},
		{
			name: "tikv network is not host when pd is host",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					PD: &v1alpha1.PDSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							HostNetwork: &enable,
						},
					},
					TiKV: &v1alpha1.TiKVSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testHostNetwork(t, false, ""),
		},
		{
			name: "tikv network is not host when tidb is host",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiDB: &v1alpha1.TiDBSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							HostNetwork: &enable,
						},
					},
					TiKV: &v1alpha1.TiKVSpec{},
					PD:   &v1alpha1.PDSpec{},
				},
			},
			testSts: testHostNetwork(t, false, ""),
		},
		{
			name: "tikv delete slots",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
					Annotations: map[string]string{
						label.AnnTiKVDeleteSlots: "[0,1]",
					},
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiDB: &v1alpha1.TiDBSpec{},
					TiKV: &v1alpha1.TiKVSpec{},
					PD:   &v1alpha1.PDSpec{},
				},
			},
			testSts: testAnnotations(t, map[string]string{"delete-slots": "[0,1]"}),
		},
		{
			name: "tikv should respect resources config",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("1"),
								corev1.ResourceMemory:           resource.MustParse("2Gi"),
								corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
								corev1.ResourceStorage:          resource.MustParse("100Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("1"),
								corev1.ResourceMemory:           resource.MustParse("2Gi"),
								corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
								corev1.ResourceStorage:          resource.MustParse("100Gi"),
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: func(sts *apps.StatefulSet) {
				g := NewGomegaWithT(t)
				g.Expect(sts.Spec.VolumeClaimTemplates[0].Spec.Resources).To(Equal(corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("100Gi"),
					},
				}))
				nameToContainer := MapContainers(&sts.Spec.Template.Spec)
				tikvContainer := nameToContainer[v1alpha1.TiKVMemberType.String()]
				g.Expect(tikvContainer.Resources).To(Equal(corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("2Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("2Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				}))
				var capacityEnvVar corev1.EnvVar
				for i := range tikvContainer.Env {
					if tikvContainer.Env[i].Name == "CAPACITY" {
						capacityEnvVar = tikvContainer.Env[i]
						break
					}
				}
				g.Expect(capacityEnvVar).To(Equal(corev1.EnvVar{
					Name:  "CAPACITY",
					Value: "100GB",
				}), "Expected the CAPACITY of tikv is properly set")
			},
		},
		{
			name: "TiKV set custom env from secret",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							EnvFrom: []corev1.EnvFromSource{
								{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "secret-example",
										},
									},
								},
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testContainerEnvFrom(t, []corev1.EnvFromSource{
				{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "secret-example",
						},
					},
				},
			}, v1alpha1.TiKVMemberType),
		},
		{
			name: "TiKV additional containers",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							AdditionalContainers: []corev1.Container{customSideCarContainers[0]},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testAdditionalContainers(t, []corev1.Container{customSideCarContainers[0]}),
		},
		{
			name: "TiKV additional volumes",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							AdditionalVolumes: []corev1.Volume{{Name: "test", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: testAdditionalVolumes(t, []corev1.Volume{{Name: "test", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}),
		},
		{
			name: "tikv spec storageVolumes",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},

				Spec: v1alpha1.TidbClusterSpec{
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
					TiKV: &v1alpha1.TiKVSpec{
						StorageVolumes: []v1alpha1.StorageVolume{
							{
								Name:        "wal",
								StorageSize: "2Gi",
								MountPath:   "/var/lib/wal",
							}},
						Config: mustTiKVConfig(&v1alpha1.TiKVRaftDBConfig{
							WalDir: pointer.StringPtr("/var/lib/wal"),
						}),
					},
				},
			},
			testSts: func(sts *apps.StatefulSet) {
				g := NewGomegaWithT(t)
				q, _ := resource.ParseQuantity("2Gi")
				g.Expect(sts.Spec.VolumeClaimTemplates).To(Equal([]corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: v1alpha1.TiKVMemberType.String(),
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: v1alpha1.TiKVMemberType.String() + "-wal",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: q,
								},
							},
						},
					},
				}))
				index := len(sts.Spec.Template.Spec.Containers[0].VolumeMounts) - 1
				g.Expect(sts.Spec.Template.Spec.Containers[0].VolumeMounts[index]).To(Equal(corev1.VolumeMount{
					Name: fmt.Sprintf("%s-%s", v1alpha1.TiKVMemberType, "wal"), MountPath: "/var/lib/wal",
				}))
			},
		},
		{
			name: "tikv spec rocksdb Log storageVolumes",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
					TiKV: &v1alpha1.TiKVSpec{
						SeparateRocksDBLog:   pointer.BoolPtr(true),
						SeparateRaftLog:      pointer.BoolPtr(true),
						RocksDBLogVolumeName: "rocksdblog",
						RaftLogVolumeName:    "raftlog",
						StorageVolumes: []v1alpha1.StorageVolume{
							{
								Name:        "rocksdblog",
								StorageSize: "1Gi",
								MountPath:   "/var/log/rocksdblog",
							},
							{
								Name:        "raftlog",
								StorageSize: "1Gi",
								MountPath:   "/var/log/raftlog",
							}},
					},
				},
			},
			testSts: func(sts *apps.StatefulSet) {
				g := NewGomegaWithT(t)
				q, _ := resource.ParseQuantity("1Gi")
				g.Expect(sts.Spec.VolumeClaimTemplates).To(Equal([]corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: v1alpha1.TiKVMemberType.String(),
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: v1alpha1.TiKVMemberType.String() + "-rocksdblog",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: q,
								},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: v1alpha1.TiKVMemberType.String() + "-raftlog",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: q,
								},
							},
						},
					},
				}))
				g.Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(3))
				index := len(sts.Spec.Template.Spec.Containers[0].VolumeMounts) - 1
				g.Expect(sts.Spec.Template.Spec.Containers[0].VolumeMounts[index]).To(Equal(corev1.VolumeMount{
					Name: fmt.Sprintf("%s-%s", v1alpha1.TiKVMemberType, "rocksdblog"), MountPath: "/var/log/rocksdblog",
				}))
				g.Expect(sts.Spec.Template.Spec.Containers[0].Command[2]).To(ContainSubstring("rocksdb.info"))
				index = len(sts.Spec.Template.Spec.Containers[1].VolumeMounts) - 1
				g.Expect(sts.Spec.Template.Spec.Containers[1].VolumeMounts[index]).To(Equal(corev1.VolumeMount{
					Name: fmt.Sprintf("%s-%s", v1alpha1.TiKVMemberType, "raftlog"), MountPath: "/var/log/raftlog",
				}))
				g.Expect(sts.Spec.Template.Spec.Containers[1].Command[2]).To(ContainSubstring("raftdb.info"))
			},
		},
		{
			name: "TiKV spec readiness",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							ReadinessProbe: &v1alpha1.Probe{
								Type: pointer.StringPtr("tcp"),
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			testSts: func(sts *apps.StatefulSet) {
				g := NewGomegaWithT(t)
				g.Expect(sts.Spec.Template.Spec.Containers[0].ReadinessProbe).To(Equal(&corev1.Probe{
					ProbeHandler:        buildTiKVReadinessProbHandler(nil),
					InitialDelaySeconds: int32(10),
				}))
			},
		},
		{
			name: "TiKV VolumeReplace modifications to sts",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						Replicas:                3,
						SpareVolReplaceReplicas: pointer.Int32(1),
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
				Status: v1alpha1.TidbClusterStatus{
					TiKV: v1alpha1.TiKVStatus{
						VolReplaceInProgress: true,
					},
				},
			},
			testSts: func(sts *apps.StatefulSet) {
				g := NewGomegaWithT(t)
				g.Expect(int(*sts.Spec.Replicas)).To(Equal(4))
				g.Expect(sts.Spec.UpdateStrategy.Type).To(Equal(apps.OnDeleteStatefulSetStrategyType))
			},
		},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			sts, err := getNewTiKVSetForTidbCluster(&tt.tc, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("error %v, wantErr %v", err, tt.wantErr)
			}
			tt.testSts(sts)
		})
	}
}

func TestTiKVInitContainers(t *testing.T) {
	privileged := true
	asRoot := false
	tests := []struct {
		name             string
		tc               v1alpha1.TidbCluster
		wantErr          bool
		expectedInit     []corev1.Container
		expectedSecurity *corev1.PodSecurityContext
	}{
		{
			name: "no init container",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							PodSecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: &asRoot,
								Sysctls: []corev1.Sysctl{
									{
										Name:  "net.core.somaxconn",
										Value: "32768",
									},
									{
										Name:  "net.ipv4.tcp_syncookies",
										Value: "0",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_time",
										Value: "300",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_intvl",
										Value: "75",
									},
								},
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit: nil,
			expectedSecurity: &corev1.PodSecurityContext{
				RunAsNonRoot: &asRoot,
				Sysctls: []corev1.Sysctl{
					{
						Name:  "net.core.somaxconn",
						Value: "32768",
					},
					{
						Name:  "net.ipv4.tcp_syncookies",
						Value: "0",
					},
					{
						Name:  "net.ipv4.tcp_keepalive_time",
						Value: "300",
					},
					{
						Name:  "net.ipv4.tcp_keepalive_intvl",
						Value: "75",
					},
				},
			},
		},
		{
			name: "sysctl with init container",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							Annotations: map[string]string{
								"tidb.pingcap.com/sysctl-init": "true",
							},
							PodSecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: &asRoot,
								Sysctls: []corev1.Sysctl{
									{
										Name:  "net.core.somaxconn",
										Value: "32768",
									},
									{
										Name:  "net.ipv4.tcp_syncookies",
										Value: "0",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_time",
										Value: "300",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_intvl",
										Value: "75",
									},
								},
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit: []corev1.Container{
				{
					Name:  "init",
					Image: "busybox:1.26.2",
					Command: []string{
						"sh",
						"-c",
						"sysctl -w net.core.somaxconn=32768 net.ipv4.tcp_syncookies=0 net.ipv4.tcp_keepalive_time=300 net.ipv4.tcp_keepalive_intvl=75",
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
				},
			},
			expectedSecurity: &corev1.PodSecurityContext{
				RunAsNonRoot: &asRoot,
				Sysctls:      []corev1.Sysctl{},
			},
		},
		{
			name: "sysctl with init container",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							Annotations: map[string]string{
								"tidb.pingcap.com/sysctl-init": "true",
							},
							PodSecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: &asRoot,
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit: nil,
			expectedSecurity: &corev1.PodSecurityContext{
				RunAsNonRoot: &asRoot,
			},
		},
		{
			name: "sysctl with init container",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							Annotations: map[string]string{
								"tidb.pingcap.com/sysctl-init": "true",
							},
							PodSecurityContext: nil,
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit:     nil,
			expectedSecurity: nil,
		},
		{
			name: "sysctl without init container due to invalid annotation",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							Annotations: map[string]string{
								"tidb.pingcap.com/sysctl-init": "false",
							},
							PodSecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: &asRoot,
								Sysctls: []corev1.Sysctl{
									{
										Name:  "net.core.somaxconn",
										Value: "32768",
									},
									{
										Name:  "net.ipv4.tcp_syncookies",
										Value: "0",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_time",
										Value: "300",
									},
									{
										Name:  "net.ipv4.tcp_keepalive_intvl",
										Value: "75",
									},
								},
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit: nil,
			expectedSecurity: &corev1.PodSecurityContext{
				RunAsNonRoot: &asRoot,
				Sysctls: []corev1.Sysctl{
					{
						Name:  "net.core.somaxconn",
						Value: "32768",
					},
					{
						Name:  "net.ipv4.tcp_syncookies",
						Value: "0",
					},
					{
						Name:  "net.ipv4.tcp_keepalive_time",
						Value: "300",
					},
					{
						Name:  "net.ipv4.tcp_keepalive_intvl",
						Value: "75",
					},
				},
			},
		},
		{
			name: "no init container no securityContext",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit:     nil,
			expectedSecurity: nil,
		},
		{
			name: "Specitfy init container resourceRequirements",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tc",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ResourceRequirements: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:     resource.MustParse("150m"),
								corev1.ResourceMemory:  resource.MustParse("200Mi"),
								corev1.ResourceStorage: resource.MustParse("20G"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("150m"),
								corev1.ResourceMemory: resource.MustParse("200Mi"),
							},
						},
						ComponentSpec: v1alpha1.ComponentSpec{
							Annotations: map[string]string{
								"tidb.pingcap.com/sysctl-init": "true",
							},
							PodSecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: &asRoot,
								Sysctls: []corev1.Sysctl{
									{
										Name:  "net.core.somaxconn",
										Value: "32768",
									},
								},
							},
						},
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expectedInit: []corev1.Container{
				{
					Name:  "init",
					Image: "busybox:1.26.2",
					Command: []string{
						"sh",
						"-c",
						"sysctl -w net.core.somaxconn=32768",
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("200Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("200Mi"),
						},
					},
				},
			},
			expectedSecurity: &corev1.PodSecurityContext{
				RunAsNonRoot: &asRoot,
				Sysctls:      []corev1.Sysctl{},
			},
		},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			sts, err := getNewTiKVSetForTidbCluster(&tt.tc, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("error %v, wantErr %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.expectedInit, sts.Spec.Template.Spec.InitContainers); diff != "" {
				t.Errorf("unexpected InitContainers in Statefulset (-want, +got): %s", diff)
			}
			if tt.expectedSecurity == nil {
				if sts.Spec.Template.Spec.SecurityContext != nil {
					t.Errorf("unexpected SecurityContext in Statefulset (want nil, got %#v)", *sts.Spec.Template.Spec.SecurityContext)
				}
			} else if sts.Spec.Template.Spec.SecurityContext == nil {
				t.Errorf("unexpected SecurityContext in Statefulset (want %#v, got nil)", *tt.expectedSecurity)
			} else if diff := cmp.Diff(*(tt.expectedSecurity), *(sts.Spec.Template.Spec.SecurityContext)); diff != "" {
				t.Errorf("unexpected SecurityContext in Statefulset (-want, +got): %s", diff)
			}
		})
	}
}

func TestGetTiKVConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	updateStrategy := v1alpha1.ConfigUpdateStrategyInPlace
	testCases := []struct {
		name     string
		tc       v1alpha1.TidbCluster
		expected *corev1.ConfigMap
	}{
		{
			name: "TiKV config is nil",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expected: nil,
		},
		{
			name: "basic",
			tc: v1alpha1.TidbCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "ns",
				},
				Spec: v1alpha1.TidbClusterSpec{
					TiKV: &v1alpha1.TiKVSpec{
						ComponentSpec: v1alpha1.ComponentSpec{
							ConfigUpdateStrategy: &updateStrategy,
						},
						Config: mustTiKVConfig(&v1alpha1.TiKVConfig{
							Raftstore: &v1alpha1.TiKVRaftstoreConfig{
								SyncLog:              pointer.BoolPtr(false),
								RaftBaseTickInterval: pointer.StringPtr("1s"),
							},
							Server: &v1alpha1.TiKVServerConfig{
								GrpcKeepaliveTimeout: pointer.StringPtr("30s"),
							},
						}),
					},
					PD:   &v1alpha1.PDSpec{},
					TiDB: &v1alpha1.TiDBSpec{},
				},
			},
			expected: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo-tikv",
					Namespace: "ns",
					Labels: map[string]string{
						"app.kubernetes.io/name":       "tidb-cluster",
						"app.kubernetes.io/managed-by": "tidb-operator",
						"app.kubernetes.io/instance":   "foo",
						"app.kubernetes.io/component":  "tikv",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "pingcap.com/v1alpha1",
							Kind:       "TidbCluster",
							Name:       "foo",
							UID:        "",
							Controller: func(b bool) *bool {
								return &b
							}(true),
							BlockOwnerDeletion: func(b bool) *bool {
								return &b
							}(true),
						},
					},
				},
				Data: map[string]string{
					"startup-script": "",
					"config-file": `[server]
  grpc-keepalive-timeout = "30s"

[raftstore]
  sync-log = false
  raft-base-tick-interval = "1s"
`,
				},
			},
		},
	}

	for i := range testCases {
		tt := &testCases[i]
		t.Run(tt.name, func(t *testing.T) {
			cm, err := getTikVConfigMap(&tt.tc)
			g.Expect(err).To(Succeed())
			if tt.expected == nil {
				g.Expect(cm).To(BeNil())
				return
			}
			// startup-script is better to be tested in e2e
			cm.Data["startup-script"] = ""

			got := cm.Data["config-file"]
			want := tt.expected.Data["config-file"]
			g.Expect(toml.Equal([]byte(got), []byte(want))).To(BeTrue())
			delete(cm.Data, "config-file")
			delete(tt.expected.Data, "config-file")

			if diff := cmp.Diff(*tt.expected, *cm); diff != "" {
				t.Errorf("unexpected plugin configuration (-want, +got): %s", diff)
			}
		})
	}
}

func TestTransformTiKVConfigMap(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name                string
		waitForLockTimeout  string
		wakeUpDelayDuration string
		result              string
	}
	tests := []testcase{
		{
			name:                "under 4.0",
			waitForLockTimeout:  "1000",
			wakeUpDelayDuration: "20",
			result: `[pessimistic-txn]
  wait-for-lock-timeout = 1000
  wake-up-delay-duration = 20
`,
		},
		{
			name:                "4.0.0",
			waitForLockTimeout:  "1s",
			wakeUpDelayDuration: "20ms",
			result: `[pessimistic-txn]
  wait-for-lock-timeout = "1s"
  wake-up-delay-duration = "20ms"
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tc := newTidbClusterForTiKV()
			tc.Spec.TiKV.Config.Set("pessimistic-txn.wait-for-lock-timeout", test.waitForLockTimeout)
			tc.Spec.TiKV.Config.Set("pessimistic-txn.wake-up-delay-duration", test.wakeUpDelayDuration)
			confText, err := tc.Spec.TiKV.Config.MarshalTOML()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(test.result).Should(Equal(transformTiKVConfigMap(string(confText), tc)))
		})
	}
}

func TestTiKVBackupConfig(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name       string
		numThreads int64
		result     string
	}
	tests := []testcase{
		{
			name:       "4.0.3",
			numThreads: 24,
			result: `[backup]
  num-threads = 24
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tc := newTidbClusterForTiKV()
			tc.Spec.TiKV.Config.Set("backup.num-threads", test.numThreads)
			confText, err := tc.Spec.TiKV.Config.MarshalTOML()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(test.result).Should(Equal(string(confText)))
		})
	}
}

func newTidbClusterForTiKV() *v1alpha1.TidbCluster {
	return &v1alpha1.TidbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Namespace:   corev1.NamespaceDefault,
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.TidbClusterSpec{
			TiKV: &v1alpha1.TiKVSpec{
				ComponentSpec: v1alpha1.ComponentSpec{
					Image: "tikv-test-image",
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:     resource.MustParse("1"),
						corev1.ResourceMemory:  resource.MustParse("2Gi"),
						corev1.ResourceStorage: resource.MustParse("100Gi"),
					},
				},
				Replicas:         3,
				StorageClassName: pointer.StringPtr("my-storage-class"),
				Config:           v1alpha1.NewTiKVConfig(),
			},
			PD:   &v1alpha1.PDSpec{},
			TiDB: &v1alpha1.TiDBSpec{},
		},
	}
}

func mustTiKVConfig(x interface{}) *v1alpha1.TiKVConfigWraper {
	data, err := toml.Marshal(x)
	if err != nil {
		panic(err)
	}

	c := v1alpha1.NewTiKVConfig()
	c.UnmarshalTOML(data)

	return c
}

func TestTiKVMemberManagerApplyPiTRConfigOverride(t *testing.T) {
	type testcase struct {
		name           string
		restores       []*v1alpha1.Restore
		expectOverride bool
		expectError    bool
		tc             *v1alpha1.TidbCluster
	}

	testFn := func(test *testcase, t *testing.T) {
		t.Run(test.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			tkmm, _, _, _, _, _ := newFakeTiKVMemberManager(test.tc)

			// Add restores to the fake restore lister
			for _, restore := range test.restores {
				err := tkmm.deps.InformerFactory.Pingcap().V1alpha1().Restores().Informer().GetIndexer().Add(restore)
				g.Expect(err).NotTo(HaveOccurred())
			}

			// Create a ConfigMap to test the override
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-tikv-config",
					Namespace: test.tc.Namespace,
				},
				Data: map[string]string{
					"config-file": `[gc]
batch-keys = 512`,
				},
			}

			err := tkmm.applyPiTRConfigOverride(test.tc, cm)

			if test.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).NotTo(HaveOccurred())

			if test.expectOverride {
				// Verify that gc.ratio-threshold is set to -1.0
				g.Expect(cm.Data["config-file"]).To(ContainSubstring("ratio-threshold = -1"))
			} else {
				// Verify that gc.ratio-threshold is not set or modified
				g.Expect(cm.Data["config-file"]).NotTo(ContainSubstring("ratio-threshold"))
			}
		})
	}

	tc := newTidbClusterForTiKV()

	tests := []testcase{
		{
			name:           "no restores",
			restores:       []*v1alpha1.Restore{},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "non-PiTR restore",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "non-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModeSnapshot,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreRunning,
					},
				},
			},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "active PiTR restore for same cluster",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreRunning,
					},
				},
			},
			expectOverride: true,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "completed PiTR restore",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "completed-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreComplete,
						Conditions: []v1alpha1.RestoreCondition{
							{
								Type:   v1alpha1.RestoreComplete,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "failed PiTR restore",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "failed-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreFailed,
						Conditions: []v1alpha1.RestoreCondition{
							{
								Type:   v1alpha1.RestoreFailed,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
			},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "PiTR restore for different cluster",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-cluster-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          "other-cluster",
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreRunning,
					},
				},
			},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "PiTR restore for different namespace",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-ns-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: "other-namespace",
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreRunning,
					},
				},
			},
			expectOverride: false,
			expectError:    false,
			tc:             tc,
		},
		{
			name: "multiple PiTR restores with one active",
			restores: []*v1alpha1.Restore{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "completed-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreComplete,
						Conditions: []v1alpha1.RestoreCondition{
							{
								Type:   v1alpha1.RestoreComplete,
								Status: corev1.ConditionTrue,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "active-pitr-restore",
						Namespace: tc.Namespace,
					},
					Spec: v1alpha1.RestoreSpec{
						BR: &v1alpha1.BRConfig{
							Cluster:          tc.Name,
							ClusterNamespace: tc.Namespace,
						},
						Mode: v1alpha1.RestoreModePiTR,
					},
					Status: v1alpha1.RestoreStatus{
						Phase: v1alpha1.RestoreRunning,
					},
				},
			},
			expectOverride: true,
			expectError:    false,
			tc:             tc,
		},
	}

	for i := range tests {
		testFn(&tests[i], t)
	}
}

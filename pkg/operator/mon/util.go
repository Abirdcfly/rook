/*
Copyright 2017 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package mon for the Ceph monitors.
package mon

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rook/rook/pkg/ceph/client"
	"github.com/rook/rook/pkg/ceph/mon"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	helper "k8s.io/kubernetes/pkg/api/v1/helper"
)

// LoadClusterInfo constructs or loads a clusterinfo and returns it along with the maxMonID
func LoadClusterInfo(context *clusterd.Context, namespace string) (*mon.ClusterInfo, int, error) {

	var clusterInfo *mon.ClusterInfo
	maxMonID := -1

	secrets, err := context.Clientset.CoreV1().Secrets(namespace).Get(appName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, maxMonID, fmt.Errorf("failed to get mon secrets. %+v", err)
		}

		clusterInfo, err = mon.CreateNamedClusterInfo(context, "", namespace)
		if err != nil {
			return nil, maxMonID, fmt.Errorf("failed to create mon secrets. %+v", err)
		}

		err = createClusterAccessSecret(context.Clientset, namespace, clusterInfo)
		if err != nil {
			return nil, maxMonID, err
		}
	} else {
		clusterInfo = &mon.ClusterInfo{
			Name:          string(secrets.Data[clusterSecretName]),
			FSID:          string(secrets.Data[fsidSecretName]),
			MonitorSecret: string(secrets.Data[monSecretName]),
			AdminSecret:   string(secrets.Data[adminSecretName]),
		}
		logger.Debugf("found existing monitor secrets for cluster %s", clusterInfo.Name)
	}

	// get the existing monitor config
	clusterInfo.Monitors, maxMonID, err = loadMonConfig(context.Clientset, namespace)
	if err != nil {
		return nil, maxMonID, fmt.Errorf("failed to get mon config. %+v", err)
	}

	return clusterInfo, maxMonID, nil
}

// WriteConnectionConfig save monitor connection config to disk
func WriteConnectionConfig(context *clusterd.Context, clusterInfo *mon.ClusterInfo) error {
	// write the latest config to the config dir
	if err := mon.GenerateAdminConnectionConfig(context, clusterInfo); err != nil {
		return fmt.Errorf("failed to write connection config. %+v", err)
	}

	return nil
}

// loadMonConfig returns the monitor endpoints and maxMonID
func loadMonConfig(clientset kubernetes.Interface, namespace string) (map[string]*mon.CephMonitorConfig, int, error) {

	monEndpointMap := map[string]*mon.CephMonitorConfig{}
	maxMonID := -1

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(EndpointConfigMapName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, -1, err
		}
		// If the config map was not found, initialize the empty set of monitors
		return monEndpointMap, maxMonID, nil
	}

	// Parse the monitor List
	if info, ok := cm.Data[EndpointDataKey]; ok {
		monEndpointMap = mon.ParseMonEndpoints(info)
	}

	// Parse the max monitor id
	if id, ok := cm.Data[MaxMonIDKey]; ok {
		maxMonID, err = strconv.Atoi(id)
		if err != nil {
			logger.Errorf("invalid max mon id %s. %+v", id, err)
		}
	}

	// Make sure the max id is consistent with the current monitors
	for _, m := range monEndpointMap {
		id, _ := getMonID(m.Name)
		if maxMonID < id {
			maxMonID = id
		}
	}

	logger.Infof("loaded: maxMonID=%d, mons=%+v", maxMonID, monEndpointMap)
	return monEndpointMap, maxMonID, nil
}

// get the ID of a monitor from its name
func getMonID(name string) (int, error) {
	if strings.Index(name, appName) != 0 || len(name) < len(appName) {
		return -1, fmt.Errorf("unexpected mon name")
	}
	id, err := strconv.Atoi(name[len(appName):])
	if err != nil {
		return -1, err
	}
	return id, nil
}

func createClusterAccessSecret(clientset kubernetes.Interface, namespace string, clusterInfo *mon.ClusterInfo) error {
	logger.Infof("creating mon secrets for a new cluster")
	var err error

	// store the secrets for internal usage of the rook pods
	secrets := map[string]string{
		clusterSecretName: clusterInfo.Name,
		fsidSecretName:    clusterInfo.FSID,
		monSecretName:     clusterInfo.MonitorSecret,
		adminSecretName:   clusterInfo.AdminSecret,
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: namespace},
		StringData: secrets,
		Type:       k8sutil.RookType,
	}

	if _, err = clientset.CoreV1().Secrets(namespace).Create(secret); err != nil {
		return fmt.Errorf("failed to save mon secrets. %+v", err)
	}

	return nil
}

func monInQuorum(monitor client.MonMapEntry, quorum []int) bool {
	for _, rank := range quorum {
		if rank == monitor.Rank {
			return true
		}
	}
	return false
}

func validNode(node v1.Node, placement k8sutil.Placement) bool {
	// a node cannot be disabled
	if node.Spec.Unschedulable {
		return false
	}

	// a node matches the NodeAffinity configuration
	// ignoring `PreferredDuringSchedulingIgnoredDuringExecution` terms: they
	// should not be used to judge a node unusable
	if placement.NodeAffinity != nil && placement.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		nodeMatches := false
		for _, req := range placement.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			nodeSelector, err := helper.NodeSelectorRequirementsAsSelector(req.MatchExpressions)
			if err != nil {
				logger.Infof("failed to parse MatchExpressions: %+v, regarding as not match.", req.MatchExpressions)
				return false
			}
			if nodeSelector.Matches(labels.Set(node.Labels)) {
				nodeMatches = true
				break
			}
		}
		if !nodeMatches {
			return false
		}
	}

	// a node is tainted and cannot be tolerated
	for _, taint := range node.Spec.Taints {
		isTolerated := false
		for _, toleration := range placement.Tolerations {
			if toleration.ToleratesTaint(&taint) {
				isTolerated = true
				break
			}
		}
		if !isTolerated {
			return false
		}
	}

	// a node must be Ready
	for _, c := range node.Status.Conditions {
		if c.Type == v1.NodeReady {
			return true
		}
	}
	logger.Infof("node %s is not ready. %+v", node.Name, node.Status.Conditions)
	return false
}

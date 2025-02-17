//go:build e2e
// +build e2e

/*
Copyright 2022 The Kubernetes Authors.

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

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	aadpodv1 "github.com/Azure/aad-pod-identity/pkg/apis/aadpodidentity/v1"
	"github.com/Azure/azure-sdk-for-go/profiles/2020-09-01/monitor/mgmt/insights"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1beta1"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/framework"
)

type (
	AzureClusterProxy struct {
		framework.ClusterProxy
	}
	// myEventData is used to be able to Marshal insights.EventData into JSON
	// see https://github.com/Azure/azure-sdk-for-go/issues/8224#issuecomment-614777550
	myEventData insights.EventData
)

func NewAzureClusterProxy(name string, kubeconfigPath string, options ...framework.Option) *AzureClusterProxy {
	proxy := framework.NewClusterProxy(name, kubeconfigPath, initScheme(), options...)
	return &AzureClusterProxy{
		ClusterProxy: proxy,
	}
}

func initScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)
	Expect(infrav1.AddToScheme(scheme)).To(Succeed())
	Expect(infrav1exp.AddToScheme(scheme)).To(Succeed())
	Expect(expv1.AddToScheme(scheme)).To(Succeed())
	// Add aadpodidentity v1 to the scheme.
	aadPodIdentityGroupVersion := schema.GroupVersion{Group: aadpodv1.GroupName, Version: "v1"}
	scheme.AddKnownTypes(aadPodIdentityGroupVersion,
		&aadpodv1.AzureIdentity{},
		&aadpodv1.AzureIdentityList{},
		&aadpodv1.AzureIdentityBinding{},
		&aadpodv1.AzureIdentityBindingList{},
		&aadpodv1.AzureAssignedIdentity{},
		&aadpodv1.AzureAssignedIdentityList{},
		&aadpodv1.AzurePodIdentityException{},
		&aadpodv1.AzurePodIdentityExceptionList{},
	)
	metav1.AddToGroupVersion(scheme, aadPodIdentityGroupVersion)
	return scheme
}

func (acp *AzureClusterProxy) CollectWorkloadClusterLogs(ctx context.Context, namespace, name, outputPath string) {
	Logf("Dumping workload cluster %s/%s logs", namespace, name)
	acp.ClusterProxy.CollectWorkloadClusterLogs(ctx, namespace, name, outputPath)

	aboveMachinesPath := strings.Replace(outputPath, "/machines", "", 1)

	Logf("Dumping workload cluster %s/%s kube-system pod logs", namespace, name)
	start := time.Now()
	acp.collectPodLogs(ctx, namespace, name, aboveMachinesPath)
	Logf("Fetching kube-system pod logs took %s", time.Since(start).String())

	Logf("Dumping workload cluster %s/%s Azure activity log", namespace, name)
	start = time.Now()
	acp.collectActivityLogs(ctx, namespace, name, aboveMachinesPath)
	Logf("Fetching activity logs took %s", time.Since(start).String())
}

func (acp *AzureClusterProxy) collectPodLogs(ctx context.Context, namespace string, name string, aboveMachinesPath string) {
	workload := acp.GetWorkloadCluster(ctx, namespace, name)
	pods := &corev1.PodList{}

	Expect(workload.GetClient().List(ctx, pods)).To(Succeed())

	var events = make(map[string]*corev1.EventList)
	var err error

	for _, pod := range pods.Items {
		podNamespace := pod.GetNamespace()

		// Collect events for Pod.
		if _, ok := events[podNamespace]; !ok {
			events[podNamespace], err = workload.GetClientSet().CoreV1().Events(podNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				Logf("failed to get events in %s namespace: %v", podNamespace, err)
			}
		}

		var eventMsgs string

		for _, event := range events[podNamespace].Items {
			if event.InvolvedObject.Kind == "Pod" && event.InvolvedObject.Name == pod.GetName() {
				eventMsgs += fmt.Sprintf("%s\n", event.Message)
			}
		}

		for _, container := range pod.Spec.Containers {
			// Watch each container's logs in a goroutine so we can stream them all concurrently.
			go func(pod corev1.Pod, container corev1.Container) {
				defer GinkgoRecover()

				Logf("Creating log watcher for controller %s/%s, container %s", podNamespace, pod.Name, container.Name)
				logFile := path.Join(aboveMachinesPath, podNamespace, pod.Name, container.Name+".log")
				if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
					// Failing to mkdir should not cause the test to fail
					Logf("Error mkdir: %v", err)
					return
				}

				f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					// Failing to fetch logs should not cause the test to fail
					Logf("Error opening file to write pod logs: %v", err)
					return
				}
				defer f.Close()

				opts := &corev1.PodLogOptions{
					Container: container.Name,
					Follow:    true,
				}

				podLogs, err := workload.GetClientSet().CoreV1().Pods(podNamespace).GetLogs(pod.Name, opts).Stream(ctx)
				if err != nil {
					// Failing to stream logs should not cause the test to fail
					Logf("Error starting logs stream for pod %s/%s, container %s: %v", podNamespace, pod.Name, container.Name, err)
					return
				}
				defer podLogs.Close()

				out := bufio.NewWriter(f)
				defer out.Flush()
				_, err = out.ReadFrom(podLogs)
				if errors.Is(err, io.ErrUnexpectedEOF) {
					// Failing to stream logs should not cause the test to fail
					Logf("Got error while streaming logs for pod %s/%s, container %s: %v", podNamespace, pod.Name, container.Name, err)
				}
			}(pod, container)
		}

		go func(pod corev1.Pod) {
			defer GinkgoRecover()

			Logf("Collecting events for Pod %s/%s", podNamespace, pod.Name)
			eventFile := path.Join(aboveMachinesPath, podNamespace, pod.Name, "pod-events.txt")
			if err := os.MkdirAll(filepath.Dir(eventFile), 0o755); err != nil {
				// Failing to mkdir should not cause the test to fail
				Logf("Error mkdir: %v", err)
				return
			}

			f, err := os.OpenFile(eventFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				// Failing to open the file should not cause the test to fail
				Logf("Error opening file to write Pod events: %v", err)
				return
			}
			defer f.Close()

			out := bufio.NewWriter(f)
			defer out.Flush()
			_, err = out.WriteString(eventMsgs)
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// Failing to collect event message should not cause the test to fail
				Logf("failed to collect event message of pod %s/%s: %v", podNamespace, pod.Name, err)
			}
		}(pod)
	}
}

func (acp *AzureClusterProxy) collectActivityLogs(ctx context.Context, namespace, name, aboveMachinesPath string) {
	timeoutctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	settings, err := auth.GetSettingsFromEnvironment()
	Expect(err).NotTo(HaveOccurred())
	subscriptionID := settings.GetSubscriptionID()
	authorizer, err := settings.GetAuthorizer()
	Expect(err).NotTo(HaveOccurred())
	activityLogsClient := insights.NewActivityLogsClient(subscriptionID)
	activityLogsClient.Authorizer = authorizer

	var groupName string
	clusterClient := acp.GetClient()
	workloadCluster, err := getAzureCluster(timeoutctx, clusterClient, namespace, name)
	if apierrors.IsNotFound(err) {
		controlPlane, err := getAzureManagedControlPlane(timeoutctx, clusterClient, namespace, name)
		if err != nil {
			// Failing to fetch logs should not cause the test to fail
			Logf("Error fetching activity logs for cluster %s in namespace %s.  Not able to find the AzureManagedControlPlane on the management cluster: %v", name, namespace, err)
			return
		}
		groupName = controlPlane.Spec.ResourceGroupName
	} else {
		if err != nil {
			// Failing to fetch logs should not cause the test to fail
			Logf("Error fetching activity logs for cluster %s in namespace %s.  Not able to find the workload cluster on the management cluster: %v", name, namespace, err)
			return
		}
		groupName = workloadCluster.Spec.ResourceGroup
	}

	start := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().UTC().Format(time.RFC3339)

	itr, err := activityLogsClient.ListComplete(timeoutctx, fmt.Sprintf("eventTimestamp ge '%s' and eventTimestamp le '%s' and resourceGroupName eq '%s'", start, end, groupName), "")
	if err != nil {
		// Failing to fetch logs should not cause the test to fail
		Logf("Error fetching activity logs for resource group %s: %v", groupName, err)
		return
	}

	logFile := path.Join(aboveMachinesPath, activitylog, groupName+".log")
	Expect(os.MkdirAll(filepath.Dir(logFile), 0o755)).To(Succeed())

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// Failing to fetch logs should not cause the test to fail
		Logf("Error opening file to write activity logs: %v", err)
		return
	}
	defer f.Close()
	out := bufio.NewWriter(f)
	defer out.Flush()

	for ; itr.NotDone(); err = itr.NextWithContext(timeoutctx) {
		if err != nil {
			Logf("Got error while iterating over activity logs for resource group %s: %v", groupName, err)
			return
		}
		event := itr.Value()
		if to.String(event.Category.Value) != "Policy" {
			b, err := json.MarshalIndent(myEventData(event), "", "    ")
			if err != nil {
				Logf("Got error converting activity logs data to json: %v", err)
			}
			if _, err = out.WriteString(string(b) + "\n"); err != nil {
				Logf("Got error while writing activity logs for resource group %s: %v", groupName, err)
			}
		}
	}
}

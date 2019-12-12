/*
Copyright (C) 2018 Yunify, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this work except in compliance with the License.
You may obtain a copy of the License in the LICENSE file, or at:

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package test

import (
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	typedstoragev1 "k8s.io/client-go/kubernetes/typed/storage/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/kubectl/pkg/describe/versioned"

	"path/filepath"
	"testing"
)

var (

	clientSet  *kubernetes.Clientset
	restConfig *rest.Config

	scClient         typedstoragev1.StorageClassInterface
	nodeClient       typedcorev1.NodeInterface
	podClient        typedcorev1.PodInterface
	deploymentClient typedappsv1.DeploymentInterface
	pvcClient        typedcorev1.PersistentVolumeClaimInterface

	podDescriber *versioned.PodDescriber
	pvcDescriber *versioned.PersistentVolumeClaimDescriber

)

var _ = BeforeSuite(func() {
	var err error
	restConfig, err = clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config-neonsan" ))
	Expect(err).NotTo(HaveOccurred())

	clientSet ,err = kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())
	Expect(clientSet).NotTo(BeNil())

	scClient = clientSet.StorageV1().StorageClasses()
	nodeClient = clientSet.CoreV1().Nodes()
	podClient = clientSet.CoreV1().Pods(corev1.NamespaceDefault)
	deploymentClient = clientSet.AppsV1().Deployments(corev1.NamespaceDefault)
	pvcClient = clientSet.CoreV1().PersistentVolumeClaims(corev1.NamespaceDefault)

	podDescriber = &versioned.PodDescriber{Interface: clientSet}
	pvcDescriber = &versioned.PersistentVolumeClaimDescriber{Interface: clientSet}

})

func createPVCAndDeployment(pvcName, deploymentName, scName string) (pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) {

	_, err := scClient.Get(scName, metav1.GetOptions{})
	if err != nil {
		Skip(fmt.Sprintf("storage class: %s not defined", scName))
	}

	By("Create PVC")
	pvc = newPVC(pvcName, scName)
	pvcCreate, err := pvcClient.Create(pvc)
	Expect(err).NotTo(HaveOccurred())
	printYaml("pvcCreate", pvcCreate)

	By("Create Deployment")
	deployment := newDeployment(deploymentName, pvcName, scName)
	deploymentCreate, err := deploymentClient.Create(deployment)
	Expect(err).NotTo(HaveOccurred())
	printYaml("deploymentCreate", deploymentCreate)

	By("Get pod")
	pod, err = podOfDeployment(podClient, deploymentName)
	Expect(err).NotTo(HaveOccurred())
	describePod(podDescriber, pod)

	By("Get PVC")
	pvc, err = pvcClient.Get(pvcName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	//describePvc(pvcDescriber, pvc)

	By("Volume size in pod")
	diskSize, err := podVolumeSize(clientSet, restConfig, pod)
	Expect(err).NotTo(HaveOccurred())
	pvcSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	Expect(diskSize).To(Equal(pvcSize.String()))
	return
}

func deleteDeploymentAndPVC(pvcName, deploymentName string) {
	By("Delete Deployment")
	err := deploymentClient.Delete(deploymentName, nil)
	Expect(err).NotTo(HaveOccurred())

	By("Delete PVC")
	err = pvcClient.Delete(pvcName, nil)
	Expect(err).NotTo(HaveOccurred())
}

func csiBase(scName string) {
	pvcName := fmt.Sprintf("pvc-%s-base", scName)
	deploymentName := fmt.Sprintf("deployment-%s-base", scName)

	createPVCAndDeployment(pvcName, deploymentName, scName)
	defer func() {
		deleteDeploymentAndPVC(pvcName, deploymentName)
	}()
}

func podTransfer(scName string) {
	pvcName := fmt.Sprintf("pvc-%s-transfer", scName)
	deploymentName := fmt.Sprintf("deployment-%s-transfer", scName)

	pvc, pod := createPVCAndDeployment(pvcName, deploymentName, scName)
	defer func() {
		deleteDeploymentAndPVC(pvcName, deploymentName)
	}()

	By("Cordon node")
	err := unScheduleNode(nodeClient, pod.Spec.NodeName, true)
	Expect(err).NotTo(HaveOccurred())

	defer func() {
		By("UnCordon node")
		err = unScheduleNode(nodeClient, pod.Spec.NodeName, false)
		Expect(err).NotTo(HaveOccurred())
	}()

	By("Delete pod")
	err = podClient.Delete(pod.Name, &metav1.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())

	By("Get new pod after delete")
	newPod, err := podOfDeployment(podClient, deploymentName, pod.Name)
	Expect(err).NotTo(HaveOccurred())
	Expect(newPod).NotTo(BeNil())
	Expect(newPod.Spec.NodeName).NotTo(Equal(pod.Spec.NodeName))
	describePod(podDescriber, newPod)
	describePvc(pvcDescriber, pvc)
}

func expandOffline(scName string) {
	pvcName := fmt.Sprintf("pvc-%s-expand", scName)
	deploymentName := fmt.Sprintf("deployment-%s-expand", scName)

	pvc, pod := createPVCAndDeployment(pvcName, deploymentName, scName)
	defer func() {
		deleteDeploymentAndPVC(pvcName, deploymentName)
	}()

	By("Scale deployment 0")
	err := scaleDeployment(deploymentClient, deploymentName, 0)

	By("Update size of PVC")
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *resource.NewQuantity(volumeExpandSize, resource.BinarySI)
	_, err = pvcClient.Update(pvc)
	Expect(err).NotTo(HaveOccurred())

	By("Scale deployment 1")
	err = scaleDeployment(deploymentClient, deploymentName, 1)
	Expect(err).NotTo(HaveOccurred())

	By("Volume size in pod after expand")
	newPod, err := podOfDeployment(podClient, deploymentName, pod.Name)
	diskSize, err := podVolumeSize(clientSet, restConfig, newPod)
	Expect(err).NotTo(HaveOccurred())
	pvcSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	Expect(diskSize).To(Equal(pvcSize.String()))

}

var _ = Describe("csi-qingcloud", func() {
	scName := "csi-qingcloud"
	//scName := "local"
	It("csi base", func() {
		csiBase(scName)
	})
	It("pod transfer", func() {
		podTransfer(scName)
	})
	It("expand offline", func() {
		expandOffline(scName)
	})
})

var _ = Describe("csi-neonsan", func() {
	scName := "csi-neonsan"
	It("csi base", func() {
		csiBase(scName)
	})
	It("pod transfer", func() {
		podTransfer(scName)
	})
	It("expand offline", func() {
		expandOffline(scName)
	})
})

func Test_CSI_E2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CSI Sanity Test Suite")
}

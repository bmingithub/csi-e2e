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
	"bytes"
	"fmt"
	"github.com/avast/retry-go"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/describe"
	"k8s.io/kubectl/pkg/describe/versioned"
	"os"
	"regexp"
	"sigs.k8s.io/yaml"
	"strings"
	"time"
)

const (
	volumeSize = 20 << 30
	volumeExpandSize = 40 << 30
	mountPath  = "/mnt"
	containerName = "nginx"
)

func int32Ptr(i int32) *int32 { return &i }

func newClientSet(configFile string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", configFile)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func newPVC(pvcName, scName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(volumeSize, resource.BinarySI),
				},
			},
			StorageClassName: &scName,
		},
	}
}

func newDeployment(deployName,  pvcName, scName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployName,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":  deployName,
					"tier": scName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":  deployName,
						"tier": scName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            containerName,
							Image:           "nginx",
							ImagePullPolicy: corev1.PullIfNotPresent,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "pvc-nginx",
									MountPath: mountPath,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "pvc-nginx",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
									ReadOnly:  false,
								},
							},
						},
					},
				},
			},
		},
	}
}

func podOfDeployment(podClient typedcorev1.PodInterface, deploymentName string, excludePodNames ...string) (pod *corev1.Pod, err error) {
	retryCnt := 0
	err = retry.Do(func() error {
		podList, err := podClient.List(metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", deploymentName),
		})
		if err != nil {
			return err
		}
		if podList == nil {
			return fmt.Errorf("podList == nil")
		}
		pod = &podList.Items[0]

		if len(excludePodNames) > 0 {
			for _, name := range excludePodNames {
				if name == pod.Name {
					return fmt.Errorf("pod %s excluded", pod.Name)
				}
			}
		}
		retryCnt++
		fmt.Printf("get pod of %s ,retry %d time(s)\n", deploymentName, retryCnt)
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod:%s,  not running", pod.Status.Phase)
		}
		return nil
	}, retry.Delay(time.Second * 3 ), retry.DelayType(retry.FixedDelay),retry.Attempts(100 ))

	fmt.Printf("\n")
	return
}

func unScheduleNode(nodeClient typedcorev1.NodeInterface, nodeName string, unscheduable bool) error {
	node, err := nodeClient.Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	node.Spec.Unschedulable = unscheduable
	node, err = nodeClient.Update(node)
	if err != nil {
		return err
	}
	return nil
}

func scaleDeployment(deploymentClient typedappsv1.DeploymentInterface, deploymentName string, replicas int32) error {
	scale, err := deploymentClient.GetScale(deploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = deploymentClient.UpdateScale(deploymentName, scale)
	if err != nil {
		return err
	}
	return nil
}

func podVolumeSize(clientSet *kubernetes.Clientset, restConfig *rest.Config,  pod *corev1.Pod) (string,error) {
	stdOut, _, err := execToPodThroughAPI(clientSet, restConfig, "df -h", containerName, pod, os.Stdin)
	if err != nil{
		return "", err
	}
	fmt.Println(stdOut)
	return pathSize(stdOut, mountPath),nil
}

func pathSize(dfOut, mountPath string) string {
	in := strings.Trim(dfOut, "\n")
	lines := strings.Split(in, "\n")
	for _, line := range lines {
		fields := regexp.MustCompile("\\s+").Split(line, -1)
		if len(fields) > 5 && fields[5] == mountPath {
			return fmt.Sprintf("%si",fields[1])
		}
	}
	return ""
}

func execToPodThroughAPI(clientSet *kubernetes.Clientset,restConfig *rest.Config, command, containerName string, p *corev1.Pod, stdin io.Reader) (string, string, error) {
	req := clientSet.CoreV1().RESTClient().Post().Resource("pods").
		Name(p.Name).Namespace(p.Namespace).SubResource("exec")

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return "", "", fmt.Errorf("error adding to scheme: %v", err)
	}

	parameterCodec := runtime.NewParameterCodec(scheme)
	req.VersionedParams(&corev1.PodExecOptions{
		Command:   strings.Fields(command),
		Container: containerName,
		Stdin:     stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, parameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig,"POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("error while creating Executor: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return "", "", fmt.Errorf("error in Stream: %v", err)
	}

	return stdout.String(), stderr.String(), nil
}

func printYaml(name string, o interface{}) {
	b, err := yaml.Marshal(o)
	if err != nil {
		fmt.Println(o, " yaml marshal err: ", err)
		return
	}
	fmt.Printf("---------------- %s ----------------\n", name)
	fmt.Println(string(b))
}

func describePvc(pvcDescriber *versioned.PersistentVolumeClaimDescriber, pvc *corev1.PersistentVolumeClaim) {
	fmt.Printf("---------------- describe PVC: %s ----------------\n", pvc.Name)
	describeInfo, err := pvcDescriber.Describe(corev1.NamespaceDefault, pvc.Name, describe.DescriberSettings{ShowEvents: true})
	if err != nil {
		fmt.Printf("describe error:%s\n", err)
	}
	fmt.Println(describeInfo)
}

func describePod(podDescriber *versioned.PodDescriber, pod *corev1.Pod) {
	fmt.Printf("---------------- describe pod: %s ----------------\n", pod.Name)
	describeInfo, err := podDescriber.Describe(corev1.NamespaceDefault, pod.Name, describe.DescriberSettings{ShowEvents: true})
	if err != nil {
		fmt.Printf("describe error:%s\n", err)
	}
	fmt.Println(describeInfo)
}

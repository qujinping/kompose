/*
Copyright 2017 The Kubernetes Authors All rights reserved.

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

package kubernetes

import (
	"fmt"
	"reflect"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fatih/structs"
	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/transformer"
	buildapi "github.com/openshift/origin/pkg/build/api"
	deployapi "github.com/openshift/origin/pkg/deploy/api"

	// install kubernetes api
	_ "k8s.io/kubernetes/pkg/api/install"
	_ "k8s.io/kubernetes/pkg/apis/extensions/install"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/extensions"

	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/kubectl"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/intstr"
	//"k8s.io/kubernetes/pkg/controller/daemon"
	"github.com/pkg/errors"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/labels"
	"sort"
	"strings"
)

// Kubernetes implements Transformer interface and represents Kubernetes transformer
type Kubernetes struct {
	// the user provided options from the command line
	Opt kobject.ConvertOptions
}

// TIMEOUT is how long we'll wait for the termination of kubernetes resource to be successful
// used when undeploying resources from kubernetes
const TIMEOUT = 300

// PVCRequestSize (Persistent Volume Claim) has default size
const PVCRequestSize = "100Mi"

// CheckUnsupportedKey checks if given komposeObject contains
// keys that are not supported by this transformer.
// list of all unsupported keys are stored in unsupportedKey variable
// returns list of TODO: ....
func (k *Kubernetes) CheckUnsupportedKey(komposeObject *kobject.KomposeObject, unsupportedKey map[string]bool) []string {
	// collect all keys found in project
	var keysFound []string

	for _, serviceConfig := range komposeObject.ServiceConfigs {
		// this reflection is used in check for empty arrays
		val := reflect.ValueOf(serviceConfig)
		s := structs.New(serviceConfig)

		for _, f := range s.Fields() {
			// Check if given key is among unsupported keys, and skip it if we already saw this key
			if alreadySaw, ok := unsupportedKey[f.Name()]; ok && !alreadySaw {

				if f.IsExported() && !f.IsZero() {
					// IsZero returns false for empty array/slice ([])
					// this check if field is Slice, and then it checks its size
					if field := val.FieldByName(f.Name()); field.Kind() == reflect.Slice {
						if field.Len() == 0 {
							// array is empty it doesn't matter if it is in unsupportedKey or not
							continue
						}
					}
					//get tag from kobject service configure
					tag := f.Tag(komposeObject.LoadedFrom)
					keysFound = append(keysFound, tag)
					unsupportedKey[f.Name()] = true
				}
			}
		}
	}
	return keysFound
}

// InitPodSpec creates the pod specification
func (k *Kubernetes) InitPodSpec(name string, image string) api.PodSpec {
	pod := api.PodSpec{
		Containers: []api.Container{
			{
				Name:  name,
				Image: image,
			},
		},
	}
	return pod
}

// InitRC initializes Kubernetes ReplicationController object
func (k *Kubernetes) InitRC(name string, service kobject.ServiceConfig, replicas int) *api.ReplicationController {
	rc := &api.ReplicationController{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "ReplicationController",
			APIVersion: "v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: api.ReplicationControllerSpec{
			Replicas: int32(replicas),
			Template: &api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Labels: transformer.ConfigLabels(name),
				},
				Spec: k.InitPodSpec(name, service.Image),
			},
		},
	}
	return rc
}

// InitSvc initializes Kubernetes Service object
func (k *Kubernetes) InitSvc(name string, service kobject.ServiceConfig) *api.Service {
	svc := &api.Service{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: api.ServiceSpec{
			Selector: transformer.ConfigLabels(name),
		},
	}
	return svc
}

// InitD initializes Kubernetes Deployment object
func (k *Kubernetes) InitD(name string, service kobject.ServiceConfig, replicas int) *extensions.Deployment {
	dc := &extensions.Deployment{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: extensions.DeploymentSpec{
			Replicas: int32(replicas),
			Template: api.PodTemplateSpec{
				Spec: k.InitPodSpec(name, service.Image),
			},
		},
	}
	return dc
}

// InitDS initializes Kubernetes DaemonSet object
func (k *Kubernetes) InitDS(name string, service kobject.ServiceConfig) *extensions.DaemonSet {
	ds := &extensions.DaemonSet{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: extensions.DaemonSetSpec{
			Template: api.PodTemplateSpec{
				Spec: k.InitPodSpec(name, service.Image),
			},
		},
	}
	return ds
}

func (k *Kubernetes) initIngress(name string, service kobject.ServiceConfig, port int32) *extensions.Ingress {

	ingress := &extensions.Ingress{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: extensions.IngressSpec{
			Rules: []extensions.IngressRule{
				{
					IngressRuleValue: extensions.IngressRuleValue{
						HTTP: &extensions.HTTPIngressRuleValue{
							Paths: []extensions.HTTPIngressPath{
								{
									Backend: extensions.IngressBackend{
										ServiceName: name,
										ServicePort: intstr.IntOrString{
											IntVal: port,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if service.ExposeService != "true" {
		ingress.Spec.Rules[0].Host = service.ExposeService
	}

	return ingress
}

// CreatePVC initializes PersistentVolumeClaim
func (k *Kubernetes) CreatePVC(name string, mode string) (*api.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(PVCRequestSize)
	if err != nil {
		return nil, errors.Wrap(err, "resource.ParseQuantity failed, Error parsing size")
	}

	pvc := &api.PersistentVolumeClaim{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: api.PersistentVolumeClaimSpec{
			Resources: api.ResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceStorage: size,
				},
			},
		},
	}

	if mode == "ro" {
		pvc.Spec.AccessModes = []api.PersistentVolumeAccessMode{api.ReadOnlyMany}
	} else {
		pvc.Spec.AccessModes = []api.PersistentVolumeAccessMode{api.ReadWriteOnce}
	}
	return pvc, nil
}

// ConfigPorts configures the container ports.
func (k *Kubernetes) ConfigPorts(name string, service kobject.ServiceConfig) []api.ContainerPort {
	ports := []api.ContainerPort{}
	for _, port := range service.Port {

		// If the default is already TCP, no need to include it.
		if port.Protocol == api.ProtocolTCP {
			ports = append(ports, api.ContainerPort{
				ContainerPort: port.ContainerPort,
				HostIP:        port.HostIP,
			})
		} else {
			ports = append(ports, api.ContainerPort{
				ContainerPort: port.ContainerPort,
				Protocol:      port.Protocol,
				HostIP:        port.HostIP,
			})
		}

	}

	return ports
}

// ConfigServicePorts configure the container service ports.
func (k *Kubernetes) ConfigServicePorts(name string, service kobject.ServiceConfig) []api.ServicePort {
	servicePorts := []api.ServicePort{}
	for _, port := range service.Port {
		if port.HostPort == 0 {
			port.HostPort = port.ContainerPort
		}

		var targetPort intstr.IntOrString
		targetPort.IntVal = port.ContainerPort
		targetPort.StrVal = strconv.Itoa(int(port.ContainerPort))

		// If the default is already TCP, no need to include it.
		if port.Protocol == api.ProtocolTCP {
			servicePorts = append(servicePorts, api.ServicePort{
				Name:       strconv.Itoa(int(port.HostPort)),
				Port:       port.HostPort,
				TargetPort: targetPort,
			})
		} else {
			servicePorts = append(servicePorts, api.ServicePort{
				Name:       strconv.Itoa(int(port.HostPort)),
				Protocol:   port.Protocol,
				Port:       port.HostPort,
				TargetPort: targetPort,
			})
		}
	}
	return servicePorts
}

//ConfigCapabilities configure POSIX capabilities that can be added or removed to a container
func (k *Kubernetes) ConfigCapabilities(service kobject.ServiceConfig) *api.Capabilities {
	capsAdd := []api.Capability{}
	capsDrop := []api.Capability{}
	for _, capAdd := range service.CapAdd {
		capsAdd = append(capsAdd, api.Capability(capAdd))
	}
	for _, capDrop := range service.CapDrop {
		capsDrop = append(capsDrop, api.Capability(capDrop))
	}
	return &api.Capabilities{
		Add:  capsAdd,
		Drop: capsDrop,
	}
}

// ConfigTmpfs configure the tmpfs.
func (k *Kubernetes) ConfigTmpfs(name string, service kobject.ServiceConfig) ([]api.VolumeMount, []api.Volume) {
	//initializing volumemounts and volumes
	volumeMounts := []api.VolumeMount{}
	volumes := []api.Volume{}

	for index, volume := range service.TmpFs {
		//naming volumes if multiple tmpfs are provided
		volumeName := fmt.Sprintf("%s-tmpfs%d", name, index)

		// create a new volume mount object and append to list
		volMount := api.VolumeMount{
			Name:      volumeName,
			MountPath: volume,
		}
		volumeMounts = append(volumeMounts, volMount)

		//create tmpfs specific empty volumes
		volSource := k.ConfigEmptyVolumeSource("tmpfs")

		// create a new volume object using the volsource and add to list
		vol := api.Volume{
			Name:         volumeName,
			VolumeSource: *volSource,
		}
		volumes = append(volumes, vol)
	}
	return volumeMounts, volumes
}

// ConfigVolumes configure the container volumes.
func (k *Kubernetes) ConfigVolumes(name string, service kobject.ServiceConfig) ([]api.VolumeMount, []api.Volume, []*api.PersistentVolumeClaim, error) {
	volumeMounts := []api.VolumeMount{}
	volumes := []api.Volume{}
	var PVCs []*api.PersistentVolumeClaim
	var volumeName string

	// Set a var based on if the user wants to use empty volumes
	// as opposed to persistent volumes and volume claims
	useEmptyVolumes := k.Opt.EmptyVols

	var count int
	//interating over array of `Vols` struct as it contains all necessary information about volumes
	for _, volume := range service.Volumes {

		// check if ro/rw mode is defined, default rw
		readonly := len(volume.Mode) > 0 && volume.Mode == "ro"

		if volume.VolumeName == "" {
			if useEmptyVolumes {
				volumeName = strings.Replace(volume.PVCName, "claim", "empty", 1)
			} else {
				volumeName = volume.PVCName
			}
			count++
		} else {
			volumeName = volume.VolumeName
		}
		volmount := api.VolumeMount{
			Name:      volumeName,
			ReadOnly:  readonly,
			MountPath: volume.Container,
		}
		volumeMounts = append(volumeMounts, volmount)
		// Get a volume source based on the type of volume we are using
		// For PVC we will also create a PVC object and add to list
		var volsource *api.VolumeSource

		if useEmptyVolumes {
			volsource = k.ConfigEmptyVolumeSource("volume")
		} else {

			volsource = k.ConfigPVCVolumeSource(volumeName, readonly)
			if volume.VFrom == "" {
				createdPVC, err := k.CreatePVC(volumeName, volume.Mode)

				if err != nil {
					return nil, nil, nil, errors.Wrap(err, "k.CreatePVC failed")
				}

				PVCs = append(PVCs, createdPVC)
			}
		}

		// create a new volume object using the volsource and add to list
		vol := api.Volume{
			Name:         volumeName,
			VolumeSource: *volsource,
		}
		volumes = append(volumes, vol)

		if len(volume.Host) > 0 {
			log.Warningf("Volume mount on the host %q isn't supported - ignoring path on the host", volume.Host)
		}

	}

	return volumeMounts, volumes, PVCs, nil
}

// ConfigEmptyVolumeSource is helper function to create an EmptyDir api.VolumeSource
//either for Tmpfs or for emptyvolumes
func (k *Kubernetes) ConfigEmptyVolumeSource(key string) *api.VolumeSource {
	//if key is tmpfs
	if key == "tmpfs" {
		return &api.VolumeSource{
			EmptyDir: &api.EmptyDirVolumeSource{Medium: api.StorageMediumMemory},
		}

	}

	//if key is volume
	return &api.VolumeSource{
		EmptyDir: &api.EmptyDirVolumeSource{},
	}

}

// ConfigPVCVolumeSource is helper function to create an api.VolumeSource with a PVC
func (k *Kubernetes) ConfigPVCVolumeSource(name string, readonly bool) *api.VolumeSource {
	return &api.VolumeSource{
		PersistentVolumeClaim: &api.PersistentVolumeClaimVolumeSource{
			ClaimName: name,
			ReadOnly:  readonly,
		},
	}
}

// ConfigEnvs configures the environment variables.
func (k *Kubernetes) ConfigEnvs(name string, service kobject.ServiceConfig) []api.EnvVar {
	envs := transformer.EnvSort{}
	for _, v := range service.Environment {
		envs = append(envs, api.EnvVar{
			Name:  v.Name,
			Value: v.Value,
		})
	}
	// Stable sorts data while keeping the original order of equal elements
	// we need this because envs are not populated in any random order
	// this sorting ensures they are populated in a particular order
	sort.Stable(envs)
	return envs
}

// CreateKubernetesObjects generates a Kubernetes artifact for each input type service
func (k *Kubernetes) CreateKubernetesObjects(name string, service kobject.ServiceConfig, opt kobject.ConvertOptions) []runtime.Object {
	var objects []runtime.Object
	var replica int
	if opt.IsReplicaSetFlag || service.Replicas == 0 {
		replica = opt.Replicas
	} else {
		replica = service.Replicas
	}

	if opt.CreateD {
		objects = append(objects, k.InitD(name, service, replica))
	}
	if opt.CreateDS {
		objects = append(objects, k.InitDS(name, service))
	}
	if opt.CreateRC {
		objects = append(objects, k.InitRC(name, service, replica))
	}

	return objects
}

// InitPod initializes Kubernetes Pod object
func (k *Kubernetes) InitPod(name string, service kobject.ServiceConfig) *api.Pod {
	pod := api.Pod{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: api.ObjectMeta{
			Name:   name,
			Labels: transformer.ConfigLabels(name),
		},
		Spec: k.InitPodSpec(name, service.Image),
	}
	return &pod
}

// Transform maps komposeObject to k8s objects
// returns object that are already sorted in the way that Services are first
func (k *Kubernetes) Transform(komposeObject kobject.KomposeObject, opt kobject.ConvertOptions) ([]runtime.Object, error) {

	// this will hold all the converted data
	var allobjects []runtime.Object

	sortedKeys := SortedKeys(komposeObject)
	for _, name := range sortedKeys {
		service := komposeObject.ServiceConfigs[name]
		var objects []runtime.Object

		// Must build the images before conversion (got to add service.Image in case 'image' key isn't provided
		// Check that --build is set to true
		// Check to see if there is an InputFile (required!) before we build the container
		// Check that there's actually a Build key
		// Lastly, we must have an Image name to continue
		if opt.Build == "local" && opt.InputFiles != nil && service.Build != "" {

			if service.Image == "" {
				return nil, fmt.Errorf("image key required within build parameters in order to build and push service '%s'", name)
			}

			log.Infof("Build key detected. Attempting to build and push image '%s'", service.Image)

			// Get the directory where the compose file is
			composeFileDir, err := transformer.GetComposeFileDir(opt.InputFiles)
			if err != nil {
				return nil, err
			}

			// Build the container!
			err = transformer.BuildDockerImage(service, name, composeFileDir)
			if err != nil {
				return nil, errors.Wrapf(err, "Unable to build Docker image for service %v", name)
			}

			// Push the built container to the repo!
			err = transformer.PushDockerImage(service, name)
			if err != nil {
				return nil, errors.Wrapf(err, "Unable to push Docker image for service %v", name)
			}

		}

		// If there's no "image" key, use the name of the container that's built
		if service.Image == "" {
			service.Image = name
		}

		// Generate pod only and nothing more
		if service.Restart == "no" || service.Restart == "on-failure" {
			// Error out if Controller Object is specified with restart: 'on-failure'
			if opt.IsDeploymentFlag || opt.IsDaemonSetFlag || opt.IsReplicationControllerFlag {
				return nil, errors.New("Controller object cannot be specified with restart: 'on-failure'")
			}
			pod := k.InitPod(name, service)
			objects = append(objects, pod)
		} else {
			objects = k.CreateKubernetesObjects(name, service, opt)
			// If ports not provided in configuration we will not make service
			if k.PortsExist(name, service) {
				svc := k.CreateService(name, service, objects)
				objects = append(objects, svc)

				if service.ExposeService != "" {
					objects = append(objects, k.initIngress(name, service, svc.Spec.Ports[0].Port))
				}
			} else {
				svc := k.CreateHeadlessService(name, service, objects)
				objects = append(objects, svc)
			}
		}

		k.UpdateKubernetesObjects(name, service, &objects)

		allobjects = append(allobjects, objects...)
	}

	// sort all object so Services are first
	k.SortServicesFirst(&allobjects)
	return allobjects, nil
}

// UpdateController updates the given object with the given pod template update function and ObjectMeta update function
func (k *Kubernetes) UpdateController(obj runtime.Object, updateTemplate func(*api.PodTemplateSpec) error, updateMeta func(meta *api.ObjectMeta)) (err error) {
	switch t := obj.(type) {
	case *api.ReplicationController:
		if t.Spec.Template == nil {
			t.Spec.Template = &api.PodTemplateSpec{}
		}
		err = updateTemplate(t.Spec.Template)
		if err != nil {
			return errors.Wrap(err, "updateTemplate failed")
		}
		updateMeta(&t.ObjectMeta)
	case *extensions.Deployment:
		err = updateTemplate(&t.Spec.Template)
		if err != nil {
			return errors.Wrap(err, "updateTemplate failed")
		}
		updateMeta(&t.ObjectMeta)
	case *extensions.DaemonSet:
		err = updateTemplate(&t.Spec.Template)
		if err != nil {
			return errors.Wrap(err, "updateTemplate failed")
		}
		updateMeta(&t.ObjectMeta)
	case *deployapi.DeploymentConfig:
		err = updateTemplate(t.Spec.Template)
		if err != nil {
			return errors.Wrap(err, "updateTemplate failed")
		}
		updateMeta(&t.ObjectMeta)
	case *api.Pod:
		p := api.PodTemplateSpec{
			ObjectMeta: t.ObjectMeta,
			Spec:       t.Spec,
		}
		err = updateTemplate(&p)
		if err != nil {
			return errors.Wrap(err, "updateTemplate failed")
		}
		t.Spec = p.Spec
		t.ObjectMeta = p.ObjectMeta
	case *buildapi.BuildConfig:
		updateMeta(&t.ObjectMeta)
	}
	return nil
}

// GetKubernetesClient creates the k8s Client, returns k8s client and namespace
func (k *Kubernetes) GetKubernetesClient() (*client.Client, string, error) {
	// initialize Kubernetes client
	factory := cmdutil.NewFactory(nil)
	clientConfig, err := factory.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	client := client.NewOrDie(clientConfig)

	// get namespace from config
	namespace, _, err := factory.DefaultNamespace()
	if err != nil {
		return nil, "", err
	}
	return client, namespace, nil
}

// Deploy submits deployment and svc to k8s endpoint
func (k *Kubernetes) Deploy(komposeObject kobject.KomposeObject, opt kobject.ConvertOptions) error {
	//Convert komposeObject
	objects, err := k.Transform(komposeObject, opt)

	if err != nil {
		return errors.Wrap(err, "k.Transform failed")
	}

	pvcStr := " "
	if !opt.EmptyVols {
		pvcStr = " and PersistentVolumeClaims "
	}
	log.Info("We are going to create Kubernetes Deployments, Services" + pvcStr + "for your Dockerized application. " +
		"If you need different kind of resources, use the 'kompose convert' and 'kubectl create -f' commands instead. \n")

	client, ns, err := k.GetKubernetesClient()
	namespace := ns
	if opt.IsNamespaceFlag {
		namespace = opt.Namespace
	}
	if err != nil {
		return err
	}

	log.Infof("Deploying application in %q namespace", namespace)

	for _, v := range objects {
		switch t := v.(type) {
		case *extensions.Deployment:
			_, err := client.Deployments(namespace).Create(t)
			if err != nil {
				return err
			}
			log.Infof("Successfully created Deployment: %s", t.Name)
		case *api.Service:
			_, err := client.Services(namespace).Create(t)
			if err != nil {
				return err
			}
			log.Infof("Successfully created Service: %s", t.Name)
		case *api.PersistentVolumeClaim:
			_, err := client.PersistentVolumeClaims(namespace).Create(t)
			if err != nil {
				return err
			}
			log.Infof("Successfully created PersistentVolumeClaim: %s of size %s. If your cluster has dynamic storage provisioning, you don't have to do anything. Otherwise you have to create PersistentVolume to make PVC work", t.Name, PVCRequestSize)
		case *extensions.Ingress:
			_, err := client.Ingress(namespace).Create(t)
			if err != nil {
				return err
			}
			log.Infof("Successfully created Ingress: %s", t.Name)
		case *api.Pod:
			_, err := client.Pods(namespace).Create(t)
			if err != nil {
				return err
			}
			log.Infof("Successfully created Pod: %s", t.Name)
		}
	}

	if !opt.EmptyVols {
		pvcStr = ",pvc"
	} else {
		pvcStr = ""
	}
	fmt.Println("\nYour application has been deployed to Kubernetes. You can run 'kubectl get deployment,svc,pods" + pvcStr + "' for details.")

	return nil
}

// Undeploy deletes deployed objects from Kubernetes cluster
func (k *Kubernetes) Undeploy(komposeObject kobject.KomposeObject, opt kobject.ConvertOptions) []error {
	var errorList []error
	//Convert komposeObject
	objects, err := k.Transform(komposeObject, opt)
	if err != nil {
		errorList = append(errorList, err)
		return errorList
	}

	client, ns, err := k.GetKubernetesClient()
	namespace := ns
	if opt.IsNamespaceFlag {
		namespace = opt.Namespace
	}

	if err != nil {
		errorList = append(errorList, err)
		return errorList
	}

	log.Infof("Deleting application in %q namespace", namespace)

	for _, v := range objects {
		label := labels.SelectorFromSet(labels.Set(map[string]string{transformer.Selector: v.(meta.Object).GetName()}))
		options := api.ListOptions{LabelSelector: label}
		komposeLabel := map[string]string{transformer.Selector: v.(meta.Object).GetName()}
		switch t := v.(type) {
		case *extensions.Deployment:
			//delete deployment
			deployment, err := client.Deployments(namespace).List(options)
			if err != nil {
				errorList = append(errorList, err)
				break
			}
			for _, l := range deployment.Items {
				if reflect.DeepEqual(l.Labels, komposeLabel) {
					rpDeployment, err := kubectl.ReaperFor(extensions.Kind("Deployment"), client)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					//FIXME: gracePeriod is nil
					err = rpDeployment.Stop(namespace, t.Name, TIMEOUT*time.Second, nil)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					log.Infof("Successfully deleted Deployment: %s", t.Name)

				}
			}

		case *api.Service:
			//delete svc
			svc, err := client.Services(namespace).List(options)
			if err != nil {
				errorList = append(errorList, err)
				break
			}
			for _, l := range svc.Items {
				if reflect.DeepEqual(l.Labels, komposeLabel) {
					rpService, err := kubectl.ReaperFor(api.Kind("Service"), client)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					//FIXME: gracePeriod is nil
					err = rpService.Stop(namespace, t.Name, TIMEOUT*time.Second, nil)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					log.Infof("Successfully deleted Service: %s", t.Name)

				}
			}

		case *api.PersistentVolumeClaim:
			// delete pvc
			pvc, err := client.PersistentVolumeClaims(namespace).List(options)
			if err != nil {
				errorList = append(errorList, err)
				break
			}
			for _, l := range pvc.Items {
				if reflect.DeepEqual(l.Labels, komposeLabel) {
					err = client.PersistentVolumeClaims(namespace).Delete(t.Name)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					log.Infof("Successfully deleted PersistentVolumeClaim: %s", t.Name)
				}
			}

		case *extensions.Ingress:
			// delete ingress
			ingDeleteOptions := &api.DeleteOptions{
				TypeMeta: unversioned.TypeMeta{
					Kind:       "Ingress",
					APIVersion: "extensions/v1beta1",
				},
			}
			ingress, err := client.Ingress(namespace).List(options)
			if err != nil {
				errorList = append(errorList, err)
				break
			}
			for _, l := range ingress.Items {
				if reflect.DeepEqual(l.Labels, komposeLabel) {

					err = client.Ingress(namespace).Delete(t.Name, ingDeleteOptions)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					log.Infof("Successfully deleted Ingress: %s", t.Name)
				}
			}

		case *api.Pod:
			//delete pod
			pod, err := client.Pods(namespace).List(options)
			if err != nil {
				errorList = append(errorList, err)
			}
			for _, l := range pod.Items {
				if reflect.DeepEqual(l.Labels, komposeLabel) {
					rpPod, err := kubectl.ReaperFor(api.Kind("Pod"), client)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					//FIXME: gracePeriod is nil
					err = rpPod.Stop(namespace, t.Name, TIMEOUT*time.Second, nil)
					if err != nil {
						errorList = append(errorList, err)
						break
					}
					log.Infof("Successfully deleted Pod: %s", t.Name)
				}
			}
		}
	}

	return errorList
}

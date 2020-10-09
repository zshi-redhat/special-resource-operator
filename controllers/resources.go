package controllers

import (
	"bytes"
	"context"
	"html/template"
	"sort"

	"github.com/openshift-psap/special-resource-operator/yamlutil"
	buildV1 "github.com/openshift/api/build/v1"
	imageV1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	secv1 "github.com/openshift/api/security/v1"
	configv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/pkg/errors"
	errs "github.com/pkg/errors"
	monitoringV1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

type nodes struct {
	list  *unstructured.UnstructuredList
	count int64
}

var (
	manifests    = "/etc/kubernetes/special-resource/nvidia-gpu"
	kubeclient   *kubernetes.Clientset
	configclient *configv1.ConfigV1Client

	node = nodes{
		list:  &unstructured.UnstructuredList{},
		count: 0xDEADBEEF,
	}
)

// Add3dpartyResourcesToScheme Adds 3rd party resources To the operator
func Add3dpartyResourcesToScheme(scheme *runtime.Scheme) {

	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(secv1.AddToScheme(scheme))
	utilruntime.Must(buildV1.AddToScheme(scheme))
	utilruntime.Must(imageV1.AddToScheme(scheme))
	utilruntime.Must(monitoringV1.AddToScheme(scheme))
}

func cacheNodes(r *SpecialResourceReconciler, force bool) (*unstructured.UnstructuredList, error) {

	// The initial list is what we're working with
	// a SharedInformer will update the list of nodes if
	// more nodes join the cluster.
	cached := int64(len(node.list.Items))
	if cached == node.count && !force {
		return node.list, nil
	}

	node.list.SetAPIVersion("v1")
	node.list.SetKind("NodeList")

	opts := []client.ListOption{}

	// Only filter if we have a selector set, otherwise zero nodes will be
	// returned and no labels can be extracted. Set the default worker label
	// otherwise.
	if len(r.specialresource.Spec.Node.Selector) > 0 {
		opts = append(opts, client.MatchingLabels{r.specialresource.Spec.Node.Selector: "true"})
	} else {
		opts = append(opts, client.MatchingLabels{"node-role.kubernetes.io/worker": ""})
	}

	err := r.List(context.TODO(), node.list, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "Client cannot get NodeList")
	}

	return node.list, err
}

func getHardwareConfiguration(r *SpecialResourceReconciler) (*unstructured.Unstructured, error) {

	log.Info("Looking for Hardware Configuration ConfigMap for", "SpecialResource", r.specialresource.GetName())
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")

	namespacedName := types.NamespacedName{Namespace: r.specialresource.Spec.Metadata.Namespace, Name: r.specialresource.Name}
	err := r.Get(context.TODO(), namespacedName, cm)

	log.Info(err.Error())

	if apierrors.IsNotFound(err) {
		log.Info("Hardware Configuration ConfigMap not found, creating from local repository (/opt/sro/recipes) for SpecialResource: " + r.specialresource.Name)
		manifests := "/opt/sro/recipes/" + r.specialresource.Name + "/manifests"
		return getLocalHardwareConfiguration(manifests, r.specialresource.Name)
	}

	return cm, nil
}

func getLocalHardwareConfiguration(path string, specialresource string) (*unstructured.Unstructured, error) {

	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cm.SetName(specialresource)

	manifests := getAssetsFrom(path)

	data := map[string]string{}

	for _, manifest := range manifests {
		data[string(manifest.name)] = string(manifest.content)
	}

	if err := unstructured.SetNestedStringMap(cm.Object, data, "data"); err != nil {
		return cm, errs.Wrap(err, "Couldn't update ConfigMap data field")
	}

	return cm, nil
}

// ReconcileHardwareStates Reconcile Hardware States
func ReconcileHardwareStates(r *SpecialResourceReconciler, config unstructured.Unstructured) error {

	var manifests map[string]interface{}
	var err error
	var found bool

	manifests, found, err = unstructured.NestedMap(config.Object, "data")
	exitOnErrorOrNotFound(found, err)

	states := make([]string, 0, len(manifests))
	for key := range manifests {
		states = append(states, key)
	}

	sort.Strings(states)

	for _, state := range states {

		log.Info("Executing", "State", state)
		namespacedYAML := []byte(manifests[state].(string))
		if err := createFromYAML(namespacedYAML, r, r.specialresource.Spec.Metadata.Namespace); err != nil {
			return errs.Wrap(err, "Failed to create resources")
		}
	}

	return nil
}

// ReconcileHardwareConfigurations Reconcile Hardware Configurations
func ReconcileHardwareConfigurations(r *SpecialResourceReconciler) error {

	var err error
	var config *unstructured.Unstructured

	// Leave this here, this is crucial for all following work
	// Creating and setting the working namespace for the specialresource
	// specialresource name == namespace if not metadata.namespace is set
	log.Info("Creating Namespace")
	createSpecialResourceNamespace(r)

	// Check if we have a ConfigMap deployed in the specialresrouce
	// namespace if not fallback to the local repository.
	// ConfigMap can be used to overrride the local repository manifests
	// for testing.
	log.Info("Getting Configuration")
	if config, err = getHardwareConfiguration(r); err != nil {
		return errs.Wrap(err, "Error reconciling Hardware Configuration States")
	}

	log.Info("Found Hardware Configuration States", "Name", config.GetName())

	node.list, err = cacheNodes(r, false)
	exitOnError(errs.Wrap(err, "Failed to cache Nodes"))

	getRuntimeInformation(r)
	logRuntimeInformation()

	if err := ReconcileHardwareStates(r, *config); err != nil {
		return errs.Wrap(err, "Cannot reconcile hardware states")
	}

	return nil
}

func createSpecialResourceNamespace(r *SpecialResourceReconciler) {

	ns := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ")

	if r.specialresource.Spec.Metadata.Namespace != "" {
		add := []byte(r.specialresource.Spec.Metadata.Namespace)
		ns = append(ns, add...)
	} else {
		r.specialresource.Spec.Metadata.Namespace = r.specialresource.Name
		add := []byte(r.specialresource.Spec.Metadata.Namespace)
		ns = append(ns, add...)
	}
	if err := createFromYAML(ns, r, ""); err != nil {
		log.Info("Cannot reconcile specialresource namespace, something went horribly wrong")
		exitOnError(err)
	}
}

func templateRuntimeInformation(yamlSpec *[]byte, r runtimeInformation) error {

	spec := string(*yamlSpec)

	t := template.Must(template.New("runtime").Parse(spec))
	var buff bytes.Buffer
	if err := t.Execute(&buff, runInfo); err != nil {
		return errs.Wrap(err, "Cannot templatize spec for resource info injection, check manifest")
	}
	*yamlSpec = buff.Bytes()

	return nil
}

func createFromYAML(yamlFile []byte, r *SpecialResourceReconciler, namespace string) error {

	scanner := yamlutil.NewYAMLScanner(yamlFile)

	for scanner.Scan() {

		yamlSpec := scanner.Bytes()

		// We can pass template information from the CR to the yamls
		// thats why we are running 2 passes.
		if err := templateRuntimeInformation(&yamlSpec, runInfo); err != nil {
			return errs.Wrap(err, "Cannot inject runtime information 1st pass")
		}

		if err := templateRuntimeInformation(&yamlSpec, runInfo); err != nil {
			return errs.Wrap(err, "Cannot inject runtime information 2nd pass")
		}

		obj := &unstructured.Unstructured{}
		jsonSpec, err := yaml.YAMLToJSON(yamlSpec)
		if err != nil {
			return errs.Wrap(err, "Could not convert yaml file to json"+string(yamlSpec))
		}

		err = obj.UnmarshalJSON(jsonSpec)
		exitOnError(errs.Wrap(err, "Cannot unmarshall json spec, check your manifests"))

		obj.SetNamespace(namespace)

		// We are only building a driver-container if we cannot pull the image
		// We are asuming that vendors provide pre compiled DriverContainers
		// If err == nil, build a new container, if err != nil skip it
		if err := rebuildDriverContainer(obj, r); err != nil {
			log.Info("Skipping building driver-container", "Name", obj.GetName())
			return nil
		}

		// Callbacks before CRUD will update the manifests
		if err := beforeCRUDhooks(obj, r); err != nil {
			return errs.Wrap(err, "Before CRUD hooks failed")
		}
		// Create Update Delete Patch resources
		err = CRUD(obj, r)
		exitOnError(errs.Wrap(err, "CRUD exited non-zero"))

		// Callbacks after CRUD will wait for ressource and check status
		if err := afterCRUDhooks(obj, r); err != nil {
			return errs.Wrap(err, "After CRUD hooks failed")
		}

	}

	if err := scanner.Err(); err != nil {
		return errs.Wrap(err, "Failed to scan manifest")
	}
	return nil
}

// Some resources need an updated resourceversion, during updates
func needToUpdateResourceVersion(kind string) bool {

	if kind == "SecurityContextConstraints" ||
		kind == "Service" ||
		kind == "ServiceMonitor" ||
		kind == "Route" ||
		kind == "BuildConfig" ||
		kind == "ImageStream" ||
		kind == "PrometheusRule" {
		return true
	}
	return false
}

func updateResourceVersion(req *unstructured.Unstructured, found *unstructured.Unstructured) error {

	kind := found.GetKind()

	if needToUpdateResourceVersion(kind) {
		version, fnd, err := unstructured.NestedString(found.Object, "metadata", "resourceVersion")
		exitOnErrorOrNotFound(fnd, err)

		if err := unstructured.SetNestedField(req.Object, version, "metadata", "resourceVersion"); err != nil {
			return errs.Wrap(err, "Couldn't update ResourceVersion")
		}

	}
	if kind == "Service" {
		clusterIP, fnd, err := unstructured.NestedString(found.Object, "spec", "clusterIP")
		exitOnErrorOrNotFound(fnd, err)

		if err := unstructured.SetNestedField(req.Object, clusterIP, "spec", "clusterIP"); err != nil {
			return errs.Wrap(err, "Couldn't update clusterIP")
		}
		return nil
	}
	return nil
}

// CRUD Create Update Delete Resource
func CRUD(obj *unstructured.Unstructured, r *SpecialResourceReconciler) error {

	logger := log.WithValues("Kind", obj.GetKind(), "Namespace", obj.GetNamespace(), "Name", obj.GetName())
	found := obj.DeepCopy()

	if obj.GetKind() != "Namespace" {
		if err := controllerutil.SetControllerReference(&r.specialresource, obj, r.Scheme); err != nil {
			return errs.Wrap(err, "Failed to set controller reference")
		}
	}

	err := r.Get(context.TODO(), types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, found)

	if apierrors.IsNotFound(err) {
		logger.Info("Not found, creating")
		if err := r.Create(context.TODO(), obj); err != nil {
			return errs.Wrap(err, "Couldn't Create Resource")
		}
		return nil
	}

	if apierrors.IsForbidden(err) {
		return errs.Wrap(err, "Forbidden check Role, ClusterRole and Bindings for operator")
	}

	if err != nil {
		return errs.Wrap(err, "Unexpected error")
	}
	// Not updating Pod because we can only update image and some other
	// specific minor fields.
	//
	// ServiceAccounts cannot be updated, maybe delete and create?
	if obj.GetKind() == "ServiceAccount" || obj.GetKind() == "Pod" {
		logger.Info("TODO: Found, not updating, does not work, why? Secret accumulation?")
		return nil
	}

	logger.Info("Found, updating")
	required := obj.DeepCopy()

	// required.ResourceVersion = found.ResourceVersion this is only needed
	// before we update a resource, we do not care when creating, hence
	// !leave this here!
	if err := updateResourceVersion(required, found); err != nil {
		return errs.Wrap(err, "Couldn't Update ResourceVersion")
	}

	if err := r.Update(context.TODO(), required); err != nil {
		return errs.Wrap(err, "Couldn't Update Resource")
	}

	return nil
}

func rebuildDriverContainer(obj *unstructured.Unstructured, r *SpecialResourceReconciler) error {

	logger := log.WithValues("Kind", obj.GetKind(), "Namespace", obj.GetNamespace(), "Name", obj.GetName())
	// BuildConfig are currently not triggered by an update need to delete first
	if obj.GetKind() == "BuildConfig" {
		annotations := obj.GetAnnotations()
		if vendor, ok := annotations["specialresource.openshift.io/driver-container-vendor"]; ok {
			logger.Info("driver-container-vendor", "vendor", vendor)
			if vendor == runInfo.UpdateVendor {
				logger.Info("vendor == updateVendor", "vendor", vendor, "updateVendor", runInfo.UpdateVendor)
				return nil
			}
			logger.Info("vendor != updateVendor", "vendor", vendor, "updateVendor", runInfo.UpdateVendor)
			return errs.New("vendor != updateVendor")
		}
		logger.Info("No annotation driver-container-vendor found")
		return errs.New("No driver-container-vendor found, nor vendor == updateVendor")
	}

	return nil
}
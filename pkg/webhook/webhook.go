package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"net/http"
	"strings"

	"github.com/1password/kubernetes-secrets-injector/pkg/utils"
	"github.com/1password/kubernetes-secrets-injector/version"
	"github.com/golang/glog"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

const (
	connectHostEnv  = "OP_CONNECT_HOST"
	connectTokenEnv = "OP_CONNECT_TOKEN"
	// #nosec G101
	serviceAccountTokenEnv = "OP_SERVICE_ACCOUNT_TOKEN"

	// binVolumeName is the name of the volume where the OP CLI binary is stored.
	binVolumeName = "op-bin"

	// binVolumeMountPath is the mount path where the OP CLI binary can be found.
	binVolumeMountPath = "/op/bin/"

	defaultOpCLIVersion = "2"
)

// binVolume is the shared, in-memory volume where the OP CLI binary lives.
var binVolume = corev1.Volume{
	Name: binVolumeName,
	VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory,
		},
	},
}

// binVolumeMount is the shared volume mount where the OP CLI binary lives.
var binVolumeMount = corev1.VolumeMount{
	Name:      binVolumeName,
	MountPath: binVolumeMountPath,
	ReadOnly:  true,
}

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
	tokenValue    = ""
)

const (
	injectionStatus         = "operator.1password.io/status"
	injectAnnotation        = "operator.1password.io/inject"
	versionAnnotation        = "operator.1password.io/version"
	connecttokenAnnotation   = "operator.1password.io/connect-token"
	servicetokenAnnotation   = "operator.1password.io/service-token"
)

type SecretInjector struct {
	Server *http.Server
}

// the command line parameters for configuraing the webhook
type SecretInjectorParameters struct {
	Port     int    // webhook server port
	CertFile string // path to the x509 certificate for https
	KeyFile  string // path to the x509 private key matching `CertFile`
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// Check if the pod should have secrets injected
func mutationRequired(metadata *metav1.ObjectMeta) bool {
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	status := annotations[injectionStatus]
	_, enabled := annotations[injectAnnotation]

	// if pod has not already been injected and injection has been enabled mark the pod for injection
	required := false
	if strings.ToLower(status) != "injected" && enabled {
		required = true
	}

	glog.Infof("Pod %v at namespace %v. Secret injection status: %v Secret Injection Enabled:%v", metadata.Name, metadata.Namespace, status, required)
	return required
}

// Check if the pod have annotation for token to be used to fetch secrets using op cli
func fetchTokenName(metadata *metav1.ObjectMeta) (string, string) {
    if metadata == nil {
        return "", ""
    }

    annotations := metadata.GetAnnotations()
    if annotations == nil {
        return "", ""
    }

    // Get the value of connecttokenAnnotation
    if connectTokenName, exists := annotations[connecttokenAnnotation]; exists {
        return connectTokenName, ""
    }

    // Get the value of servicetokenAnnotation if connecttokenAnnotation doesn't exist
    if serviceTokenName, exists := annotations[servicetokenAnnotation]; exists {
        return "", serviceTokenName
    }

    return "", ""
}


func addContainers(target, added []corev1.Container, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func addVolume(target, added []corev1.Volume, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []patchOperation) {
	for key, value := range added {
		if target == nil || target[key] == "" {
			target = map[string]string{}
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + key,
				Value: value,
			})
		}
	}
	return patch
}

// mutation process for injecting secrets into pods
func (s *SecretInjector) mutate(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	ctx := context.Background()
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		glog.Errorf("Could not unmarshal raw object: %v", err)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	glog.Infof("Checking if secret injection is needed for %v %s at namespace %v",
		req.Kind, pod.Name, req.Namespace)

	// determine whether to inject secrets
	if !mutationRequired(&pod.ObjectMeta) {
		glog.Infof("Secret injection not required for %s at namespace %s", pod.Name, pod.Namespace)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	containersStr := pod.Annotations[injectAnnotation]

	containers := map[string]struct{}{}

	if containersStr == "" {
		glog.Infof("No containers set for secret injection for %s/%s", pod.Namespace, pod.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}
	for _, container := range strings.Split(containersStr, ",") {
		containers[container] = struct{}{}
	}

	versionAnnotation, ok := pod.Annotations[versionAnnotation]
	if !ok {
		versionAnnotation = defaultOpCLIVersion
	}

	mutated := false

	// Added tokenValue fetch here so that we can pass this to mutateContainer. This token value will be used by initContainer
	connectTokenName, serviceTokenName := fetchTokenName(&pod.ObjectMeta)

	var tokenEnvVarName, tokenValue string

	if connectTokenName != "" {
		tokenEnvVarName = "OP_CONNECT_TOKEN"
		tokenValue = os.Getenv(connectTokenName)
		if tokenValue == "" {
			glog.Infof("Failed to fetch connect token named '%s' from environment", connectTokenName)
		}
	} else if serviceTokenName != "" {
		tokenEnvVarName = "OP_SERVICE_ACCOUNT_TOKEN"
		tokenValue = os.Getenv(serviceTokenName)
		if tokenValue == "" {
			glog.Infof("Failed to fetch service token named '%s' from environment", serviceTokenName)
		}
	} else {
		// Handle the case where neither token is set.
		tokenEnvVarName = "NOTOKEN"
		tokenValue = ""
	}

	var patch []patchOperation
	for i := range pod.Spec.InitContainers {
		c := pod.Spec.InitContainers[i]
		_, mutate := containers[c.Name]
		if !mutate {
			continue
		}
		didMutate, initContainerPatch, err := s.mutateContainer(ctx, tokenValue, &c, i)
		if err != nil {
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		if didMutate {
			mutated = true
		}
		patch = append(patch, initContainerPatch...)
	}

	for i := range pod.Spec.Containers {
		c := pod.Spec.Containers[i]
		_, mutate := containers[c.Name]
		if !mutate {
			continue
		}

		didMutate, containerPatch, err := s.mutateContainer(ctx, tokenValue, &c, i)
		if err != nil {
			glog.Error("Error occurred mutating container for secret injection: ", err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		patch = append(patch, containerPatch...)
		if didMutate {
			mutated = true
		}
	}

	if !mutated {
		glog.Infof("No containers set for secret injection for %s/%s", pod.Namespace, pod.Name)
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	// binInitContainer is the container that pulls the OP CLI and set a script for TOKEN values
	// into a shared volume mount.

	scriptContent := fmt.Sprintf("#!/bin/sh\n\nexport %s=%s\n\n$@", tokenEnvVarName, tokenValue)
    // Write this scriptContent to /tmp/set_env_and_run.sh
	err := ioutil.WriteFile("/tmp/set_env_and_run.sh", []byte(scriptContent), 0755)
	if err != nil {
		glog.Errorf("Error writing the script: %v", err)
	}

	// Debug: Read the file back and log its contents
	content, readErr := ioutil.ReadFile("/tmp/set_env_and_run.sh")
	if readErr != nil {
		glog.Errorf("Error reading the script: %v", readErr)
	} else {
		glog.Infof("Script contents: %s", content)
	}
	var binInitContainer = corev1.Container{
		Name:            "copy-op-bin",
		Image:           "1password/op:" + versionAnnotation,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"sh", "-c",
			fmt.Sprintf("cp /usr/local/bin/op %s && cp /tmp/set_env_and_run.sh %s && chmod +x %sset_env_and_run.sh",
				binVolumeMountPath, 
				binVolumeMountPath, 
				binVolumeMountPath)},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      binVolumeName,
				MountPath: binVolumeMountPath,
			},
		},
	}


	patchBytes, err := createOPCLIPatch(&pod, []corev1.Container{binInitContainer}, patch)
	if err != nil {
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	logStr := string(patchBytes)

	// For OP_CONNECT_TOKEN
	reConnectToken := regexp.MustCompile(`OP_CONNECT_TOKEN=([^ ]+)`) 
	maskedConnectStr := reConnectToken.ReplaceAllString(logStr, "OP_CONNECT_TOKEN=***MASKED***")

	// For OP_SERVICE_ACCOUNT_TOKEN
	reServiceAccountToken := regexp.MustCompile(`OP_SERVICE_ACCOUNT_TOKEN=([^ ]+)`)
	maskedServiceAccountStr := reServiceAccountToken.ReplaceAllString(maskedConnectStr, "OP_SERVICE_ACCOUNT_TOKEN=***MASKED***")

	glog.Infof("AdmissionResponse: patch=%v\n", maskedServiceAccountStr)
	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// create mutation patch for resources
func createOPCLIPatch(pod *corev1.Pod, containers []corev1.Container, patch []patchOperation) ([]byte, error) {

	annotations := map[string]string{injectionStatus: "injected"}
	patch = append(patch, addVolume(pod.Spec.Volumes, []corev1.Volume{binVolume}, "/spec/volumes")...)
	patch = append(patch, addContainers(pod.Spec.InitContainers, containers, "/spec/initContainers")...)
	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patch)
}

func isEnvVarSetup(envVarName string) func(c *corev1.Container) bool {
	return func(container *corev1.Container) bool {
		envVar := findContainerEnvVarByName(envVarName, container)
		if envVar == nil {
			glog.Infof("%s not provided", envVarName)
		}
		return envVar != nil
	}
}

func isConnectHostEnvVarSetup(container *corev1.Container) bool {
	return isEnvVarSetup(connectHostEnv)(container)
}

func isConnectTokenEnvVarSetup(container *corev1.Container) bool {
	return isEnvVarSetup(connectTokenEnv)(container)
}

func isServiceAccountEnvVarSetup(container *corev1.Container) bool {
	return isEnvVarSetup(serviceAccountTokenEnv)(container)
}

func findContainerEnvVarByName(envName string, container *corev1.Container) *corev1.EnvVar {
	for _, containerEnvVar := range container.Env {
		if containerEnvVar.Name == envName {
			return &containerEnvVar
		}
	}

	return nil
}

func checkOPCLIEnvSetup(container *corev1.Container) {
	isConnectSetup := isConnectTokenEnvVarSetup(container) && isConnectHostEnvVarSetup(container)
	isServiceAccountSetup := isServiceAccountEnvVarSetup(container)
	if isConnectSetup {
		glog.Info("OP CLI will be used with Connect")
	} else if !isConnectSetup && isServiceAccountSetup {
		glog.Info("OP CLI will be used with Service Account")
	} else {
		glog.Info("No credentials provided to authenticate OP CLI")
	}
}

func passUserAgentInformationToCLI(container *corev1.Container, containerIndex int) []patchOperation {
	userAgentEnvs := []corev1.EnvVar{
		{
			Name:  "OP_INTEGRATION_NAME",
			Value: "1Password Kubernetes Webhook",
		},
		{
			Name:  "OP_INTEGRATION_ID",
			Value: "K8W",
		},
		{
			Name:  "OP_INTEGRATION_BUILDNUMBER",
			Value: utils.MakeBuildVersion(version.Version),
		},
	}

	return setEnvironment(*container, containerIndex, userAgentEnvs, "/spec/containers")
}

// mutates the container to allow for secrets to be injected into the container via the op cli
func (s *SecretInjector) mutateContainer(cxt context.Context, tokenValue string, container *corev1.Container, containerIndex int) (bool, []patchOperation, error) {

	//  prepending op run command to the container command so that secrets are injected before the main process is started
	if len(container.Command) == 0 {
		return false, nil, fmt.Errorf("not attaching OP to the container %s: the podspec does not define a command", container.Name)
	}
	// If the secret was retrieved, prepend the command with the temporary environment setting
    if tokenValue != "" {
		// Runs a script that sets the secret environment variable
		container.Command = []string{"/bin/sh", "-c", fmt.Sprintf("%sset_env_and_run.sh && rm %sset_env_and_run.sh && %sop run -- %s", 
    		binVolumeMountPath, 
    		binVolumeMountPath, 
    		binVolumeMountPath, 
    		strings.Join(container.Command, " "))}
       
    } else {
        // Prepend the command with op run --
        container.Command = append([]string{binVolumeMountPath + "op", "run", "--"}, container.Command...)
    }

	var patch []patchOperation

	// adding the cli to the container using a volume mount
	path := fmt.Sprintf("%s/%d/volumeMounts", "/spec/containers", containerIndex)
	patch = append(patch, patchOperation{
		Op:    "add",
		Path:  path,
		Value: append(container.VolumeMounts, binVolumeMount),
	})

	// replacing the container command with a command prepended with op run
	path = fmt.Sprintf("%s/%d/command", "/spec/containers", containerIndex)
	patch = append(patch, patchOperation{
		Op:    "replace",
		Path:  path,
		Value: container.Command,
	})
    if tokenValue == "" {
		checkOPCLIEnvSetup(container)
	}
	
	//creating patch for passing User-Agent information to the CLI.
	patch = append(patch, passUserAgentInformationToCLI(container, containerIndex)...)
	return true, patch, nil
}

func setEnvironment(container corev1.Container, containerIndex int, addedEnv []corev1.EnvVar, basePath string) (patch []patchOperation) {
	first := len(container.Env) == 0
	var value interface{}
	for _, add := range addedEnv {
		path := fmt.Sprintf("%s/%d/env", basePath, containerIndex)
		value = add
		if first {
			first = false
			value = []corev1.EnvVar{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

// Serve method for secrets injector webhook
func (s *SecretInjector) Serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		glog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		glog.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *admissionv1.AdmissionResponse
	ar := admissionv1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		glog.Errorf("Can't decode body: %v", err)
		admissionResponse = &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = s.mutate(&ar)
	}

	admissionReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
	}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		glog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	glog.Infof("Ready to write response ...")

	if _, err := w.Write(resp); err != nil {
		glog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

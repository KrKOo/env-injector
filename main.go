package main

// https://gist.githubusercontent.com/erkanzileli/ee889be693fb3fbb586f4415519cf564/raw/876886f9954783e57cb352cec4d1c3c1f0bfb23c/refresh_certificates_on_runtime.go
import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	infoLogger   *log.Logger
	errorLogger  *log.Logger
	deserializer runtime.Decoder
	cmPath       string
	envVarsEmpty bool
	certPath     = "/etc/tls/tls.crt"
	keyPath      = "/etc/tls/tls.key"
)

func init() {
	infoLogger = log.New(os.Stderr, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLogger = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	deserializer = serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
}

func main() {
	cmPath_ := flag.String("cm_path", "/etc/env-cm", "Path to mounted Config Map "+
		"with environment variables to inject.")
	flag.Parse()

	cmPath = *cmPath_

	certUpdater, err := newCertificateUpdater()
	if err != nil {
		log.Fatal(err)
	}

	go certUpdater.startFollowing()

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", serveMutatePods)
	mux.HandleFunc("/health", serveHealth)

	server := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			GetCertificate: certUpdater.GetCertificateFunc,
		},
		Addr: ":8443",
	}

	infoLogger.Print("Listening on port 8443...")
	err = server.ListenAndServeTLS("", "")
	server.ListenAndServe()

	if err != nil {
		errorLogger.Println("Can't serve TLS server, exiting.")
		panic(err)
	}

}

func serveHealth(writer http.ResponseWriter, request *http.Request) {
	msg := fmt.Sprintf("healthy uri %s", request.RequestURI)
	infoLogger.Println(msg)
	writer.WriteHeader(200)
	writer.Write([]byte(msg))
}

func serveMutatePods(w http.ResponseWriter, r *http.Request) {
	infoLogger.Println("received mutation request")

	if r.Header.Get("Content-Type") != "application/json" {
		errorLogger.Println("admission request must be application/json content-type")
		panic("wrong content-type")
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		errorLogger.Println("error while reading body %s", err.Error())
		panic(err)
	}

	var admissionReviewReq admissionv1.AdmissionReview

	_, _, err = deserializer.Decode(body, nil, &admissionReviewReq)
	if err != nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("unmarshalling body into admission review object not successful: %v", err))
		return
	} else if admissionReviewReq.Request == nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("using admission review not possible: request field is nil: %v", err))
		return
	}

	pod := corev1.Pod{}
	err = json.Unmarshal(admissionReviewReq.Request.Object.Raw, &pod)
	if err != nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("deserializing job not successful: %v", err))
		return
	}

	// Check namespace of Pod if not in excluded
	excl := os.Getenv("EXCL_NS")
	if excl != "" {
		namespaces := strings.Split(excl, ",")
		for _, namespace := range namespaces {
			if namespace == pod.ObjectMeta.Namespace {
				sendResponse(admissionReviewReq, w, nil, true,
					"sending back response, spec not changed (excluded namespace)")
				return
			}

		}
	}

	var patchesM []byte
	var patches []map[string]interface{}
	envPairs := readEnvsFromConfigMap(cmPath + "/envs")

	for i, container := range pod.Spec.Containers {
		envVarsEmpty = len(container.Env) == 0
		for _, v := range envPairs {
			env := strings.Split(v, "=")
			found := false
			for _, envVar := range container.Env { // EnvFromSource?
				if envVar.Name == env[0] {
					found = true
				}
			}
			if !found {
				patches = addEnvVar(env[0], env[1], i, patches, container)
			}
		}
	}

	if len(patches) == 0 {
		sendResponse(admissionReviewReq, w, nil, true,
			"sending back response, spec not changed (no new env vars)")
		return
	}

	patchesM, err = json.Marshal(patches)
	if err != nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("marshalling patches not successful: %v", err))
		return
	}

	admissionReviewResponse := generateResponse(admissionReviewReq, patchesM, true, "")

	jout, err := json.Marshal(admissionReviewResponse)
	if err != nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("marshalling response not successful: %v", err))
		return
	}
	infoLogger.Println("sending back response, job spec changed")
	w.WriteHeader(200)
	w.Write(jout)
}

func generateResponse(admissionReviewReq admissionv1.AdmissionReview, patchesM []byte, allow bool, msg string) admissionv1.AdmissionReview {
	reviewResponse := &admissionv1.AdmissionResponse{
		UID:     admissionReviewReq.Request.UID,
		Allowed: allow,
	}

	if !allow {
		reviewResponse.Result.Status = msg
		reviewResponse.Result.Code = 403
	}
	if allow && patchesM != nil {
		patchType := admissionv1.PatchTypeJSONPatch
		reviewResponse.Patch = patchesM
		reviewResponse.PatchType = &patchType
	}

	admissionReviewResponse := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: reviewResponse,
	}

	return admissionReviewResponse
}

func sendResponse(admissionReviewReq admissionv1.AdmissionReview, w http.ResponseWriter, patchesM []byte, allow bool, msg string) {
	admissionReviewResponse := generateResponse(admissionReviewReq, patchesM, allow, msg)
	jout, err := json.Marshal(admissionReviewResponse)
	if err != nil {
		sendHeaderErrorResponse(w, fmt.Sprintf("marshalling response not successful: %v", err))
		return
	}
	infoLogger.Println(msg)
	w.WriteHeader(200)
	w.Write(jout)
}

func sendHeaderErrorResponse(w http.ResponseWriter, msg string) {
	errorLogger.Println(msg)
	w.WriteHeader(400)
	w.Write([]byte(msg))
}

func addEnvVar(envname, field string, i int, patches []map[string]interface{}, container corev1.Container) []map[string]interface{} {
	if field == "limits.cpu" {
		if container.Resources.Limits["cpu"].Format != "" {
			patches = addEnvVarFromField(envname, field, true, i, patches)
		}
	} else if field == "limits.memory" {
		if container.Resources.Limits["memory"].Format != "" {
			patches = addEnvVarFromField(envname, field, true, i, patches)
		}
	} else if field == "requests.cpu" {
		if container.Resources.Requests["cpu"].Format != "" {
			patches = addEnvVarFromField(envname, field, true, i, patches)
		}
	} else if field == "requests.memory" {
		if container.Resources.Requests["memory"].Format != "" {
			patches = addEnvVarFromField(envname, field, true, i, patches)
		}
	} else {
		patches = addVar(envname, field, i, patches)
	}
	return patches
}

func addEnvVarFromField(envname, field string, isResource bool, i int, patches []map[string]interface{}) []map[string]interface{} {
	patch := map[string]interface{}{
		"op":   "add",
		"path": fmt.Sprintf("/spec/containers/%d/env", i),
	}

	envVar := corev1.EnvVar{Name: envname}
	if !isResource {
		envVar.ValueFrom = &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: field},
		}
	} else {
		envVar.ValueFrom = &corev1.EnvVarSource{
			ResourceFieldRef: &corev1.ResourceFieldSelector{
				Resource: field,
			},
		}
	}

	if envVarsEmpty {
		patch["value"] = []corev1.EnvVar{envVar}
		envVarsEmpty = false
	} else {
		patch["path"] = fmt.Sprintf("%v/-", patch["path"])
		patch["value"] = envVar
	}
	patches = append(patches, patch)
	return patches
}

func addVar(envname string, value string, i int, patches []map[string]interface{}) []map[string]interface{} {
	patch := map[string]interface{}{
		"op":   "add",
		"path": fmt.Sprintf("/spec/containers/%d/env", i),
	}
	envVar := corev1.EnvVar{Name: envname, Value: value}
	if envVarsEmpty {
		patch["value"] = []corev1.EnvVar{envVar}
		envVarsEmpty = false
	} else {
		patch["path"] = fmt.Sprintf("%v/-", patch["path"])
		patch["value"] = envVar
	}
	patches = append(patches, patch)
	return patches
}

func readEnvsFromConfigMap(path string) []string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		errorLogger.Printf("Path " + cmPath + " does not exist, exiting\n")
		panic(errors.New("Path " + cmPath + " does not exist, exiting\n"))
	}

	file, err := os.Open(path)
	if err != nil {
		panic(errors.New("Failed to open path " + path))
	}
	fileScanner := bufio.NewScanner(file)
	fileScanner.Split(bufio.ScanLines)
	var fileLines []string
	for fileScanner.Scan() {
		fileLines = append(fileLines, fileScanner.Text())
	}
	err = file.Close()
	if err != nil {
		panic(errors.New("Failed to close path " + path))
	}

	return fileLines
}

type certificateUpdater struct {
	certMu sync.RWMutex
	cert   *tls.Certificate
}

func newCertificateUpdater() (*certificateUpdater, error) {
	result := &certificateUpdater{}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	result.cert = &cert
	return result, nil
}

func (cu *certificateUpdater) startFollowing() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	if err = watcher.Add(certPath); err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				panic(errors.New("watcher event channel closed"))
			}
			log.Println("event:", event)
			if event.Op == fsnotify.Remove {
				watcher.Remove(event.Name)
				watcher.Add(event.Name)
				if err := cu.reload(); err != nil {
					panic(fmt.Errorf("failed to reload certificates: %v", err))
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				panic(errors.New("watcher error channel closed"))
			}

			if err != nil {
				panic(fmt.Errorf("watcher error: %v", err))
			}
		}
	}
}

func (cu *certificateUpdater) reload() error {
	log.Printf("reloading TLS certificate and key from %q and %q", certPath, keyPath)
	newCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Printf("Keeping old TLS certificate because the new one could not be loaded: %v", err)
		return err
	}
	cu.certMu.Lock()
	defer cu.certMu.Unlock()
	cu.cert = &newCert
	return nil
}

func (cu *certificateUpdater) GetCertificateFunc(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cu.certMu.RLock()
	defer cu.certMu.RUnlock()
	return cu.cert, nil
}

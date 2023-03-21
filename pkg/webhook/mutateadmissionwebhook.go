package webhook

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// admissionReviewHandler is a handler to handle business logic, holding an util.Factory
type admissionReviewHandler struct {
	f         cmdutil.Factory
	clientset *kubernetes.Clientset
}

// admitv1beta1Func handles a v1beta1 admission
type admitv1beta1Func func(v1beta1.AdmissionReview) *v1beta1.AdmissionResponse

// admitv1beta1Func handles a v1 admission
type admitv1Func func(v1.AdmissionReview) *v1.AdmissionResponse

// admitHandler is a handler, for both validators and mutators, that supports multiple admission review versions
type admitHandler struct {
	v1beta1 admitv1beta1Func
	v1      admitv1Func
}

func newDelegateToV1AdmitHandler(f admitv1Func) admitHandler {
	return admitHandler{
		v1beta1: delegateV1beta1AdmitToV1(f),
		v1:      f,
	}
}

func delegateV1beta1AdmitToV1(f admitv1Func) admitv1beta1Func {
	return func(review v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
		in := v1.AdmissionReview{Request: convertAdmissionRequestToV1(review.Request)}
		out := f(in)
		return convertAdmissionResponseToV1beta1(out)
	}
}

// serve handles the http portion of a request prior to handing to an admit
// function
func serve(w http.ResponseWriter, r *http.Request, admit admitHandler) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Errorf("contentType=%s, expect application/json", contentType)
		return
	}

	log.Infof("handling request: %s", body)

	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(body, nil, nil)
	if err != nil {
		msg := fmt.Sprintf("Request could not be decoded: %v", err)
		log.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	var responseObj runtime.Object
	switch *gvk {
	case v1beta1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1beta1.AdmissionReview)
		if !ok {
			log.Errorf("Expected v1beta1.AdmissionReview but got: %T", obj)
			return
		}
		responseAdmissionReview := &v1beta1.AdmissionReview{}
		responseAdmissionReview.SetGroupVersionKind(*gvk)
		responseAdmissionReview.Response = admit.v1beta1(*requestedAdmissionReview)
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
		responseObj = responseAdmissionReview
	case v1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1.AdmissionReview)
		if !ok {
			log.Errorf("Expected v1.AdmissionReview but got: %T", obj)
			return
		}
		responseAdmissionReview := &v1.AdmissionReview{}
		responseAdmissionReview.SetGroupVersionKind(*gvk)
		responseAdmissionReview.Response = admit.v1(*requestedAdmissionReview)
		responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
		responseObj = responseAdmissionReview
	default:
		msg := fmt.Sprintf("Unsupported group version kind: %v", gvk)
		log.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	log.Infof("sending response: %v", responseObj)
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		log.Error(err)
	}
}

func Main(f cmdutil.Factory) error {
	clientset, err2 := f.KubernetesClientSet()
	if err2 != nil {
		return err2
	}
	h := &admissionReviewHandler{f: f, clientset: clientset}

	http.HandleFunc("/pods", func(w http.ResponseWriter, r *http.Request) { serve(w, r, newDelegateToV1AdmitHandler(h.admitPods)) })
	http.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })

	s := &dhcpServer{f: f, clientset: clientset}
	http.HandleFunc(config.APIRentIP, s.rentIP)
	http.HandleFunc(config.APIReleaseIP, s.releaseIP)

	cert, ok := os.LookupEnv(config.TLSCertKey)
	if !ok {
		return fmt.Errorf("can not get %s from env", config.TLSCertKey)
	}
	key, ok := os.LookupEnv(config.TLSPrivateKeyKey)
	if !ok {
		return fmt.Errorf("can not get %s from env", config.TLSPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return err
	}
	t := &tls.Config{Certificates: []tls.Certificate{pair}}
	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", 80),
		TLSConfig: t,
	}
	return server.ListenAndServeTLS("", "")
}

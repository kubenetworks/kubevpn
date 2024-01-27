package webhook

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// admissionReviewHandler is a handler to handle business logic, holding an util.Factory
type admissionReviewHandler struct {
	sync.Mutex
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
		if ptr.Deref(requestedAdmissionReview.Request.DryRun, false) {
			log.Info("Ignore dryrun")
			responseObj = &v1beta1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
				},
				Response: &v1beta1.AdmissionResponse{
					Allowed: true,
					UID:     requestedAdmissionReview.Request.UID,
				},
			}
		} else {
			responseAdmissionReview := &v1beta1.AdmissionReview{}
			responseAdmissionReview.SetGroupVersionKind(*gvk)
			responseAdmissionReview.Response = admit.v1beta1(*requestedAdmissionReview)
			responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
			responseObj = responseAdmissionReview
		}
	case v1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1.AdmissionReview)
		if !ok {
			log.Errorf("Expected v1.AdmissionReview but got: %T", obj)
			return
		}
		if ptr.Deref(requestedAdmissionReview.Request.DryRun, false) {
			log.Info("Ignore dryrun")
			responseObj = &v1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
				},
				Response: &v1.AdmissionResponse{
					Allowed: true,
					UID:     requestedAdmissionReview.Request.UID,
				},
			}
		} else {
			responseAdmissionReview := &v1.AdmissionReview{}
			responseAdmissionReview.SetGroupVersionKind(*gvk)
			responseAdmissionReview.Response = admit.v1(*requestedAdmissionReview)
			responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
			responseObj = responseAdmissionReview
		}
	default:
		msg := fmt.Sprintf("Unsupported group version kind: %v", gvk)
		log.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	log.Infof("sending response: %v", responseObj)
	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		log.Errorf("Unable to encode response: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		log.Errorf("Unable to write response: %v", err)
	}
}

func Main(f cmdutil.Factory) error {
	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}

	h := &admissionReviewHandler{f: f, clientset: clientset}
	http.HandleFunc("/pods", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, newDelegateToV1AdmitHandler(h.admitPods))
	})
	http.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	svr := &dhcpServer{f: f, clientset: clientset}
	http.HandleFunc(config.APIRentIP, svr.rentIP)
	http.HandleFunc(config.APIReleaseIP, svr.releaseIP)

	var pairs []tls.Certificate
	pairs, err = getSSLKeyPairs()
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", 80),
		TLSConfig: &tls.Config{Certificates: pairs},
	}
	return server.ListenAndServeTLS("", "")
}

func getSSLKeyPairs() ([]tls.Certificate, error) {
	cert, ok := os.LookupEnv(config.TLSCertKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSCertKey)
	}
	var key string
	key, ok = os.LookupEnv(config.TLSPrivateKeyKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate and key ,err: %v", err)
	}
	return []tls.Certificate{pair}, nil
}

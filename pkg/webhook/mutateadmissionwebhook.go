package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	v1 "k8s.io/api/admission/v1"
	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// admissionReviewHandler is a handler to handle business logic, holding an util.Factory
type admissionReviewHandler struct {
	sync.Mutex
	dhcp *dhcp.Manager
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
		plog.G(context.Background()).Errorf("ContentType=%s, expect application/json", contentType)
		return
	}

	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(body, nil, nil)
	if err != nil {
		msg := fmt.Sprintf("Request: %s could not be decoded: %v", string(body), err)
		plog.G(context.Background()).Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	var responseObj runtime.Object
	switch *gvk {
	case v1beta1.SchemeGroupVersion.WithKind("AdmissionReview"):
		requestedAdmissionReview, ok := obj.(*v1beta1.AdmissionReview)
		if !ok {
			plog.G(context.Background()).Errorf("Expected v1beta1.AdmissionReview but got: %T", obj)
			return
		}
		if ptr.Deref(requestedAdmissionReview.Request.DryRun, false) {
			plog.G(context.Background()).Info("Ignore dryrun")
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
			plog.G(context.Background()).Errorf("Expected v1.AdmissionReview but got: %T", obj)
			return
		}
		if ptr.Deref(requestedAdmissionReview.Request.DryRun, false) {
			plog.G(context.Background()).Info("Ignore dry-run")
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
		plog.G(context.Background()).Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		plog.G(context.Background()).Errorf("Unable to encode response: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plog.G(context.Background()).Infof("Sending response: %v", string(respBytes))
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(respBytes); err != nil {
		plog.G(context.Background()).Errorf("Unable to write response: %v", err)
	}
}

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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type admissionReviewHandler struct {
	sync.Mutex
	dhcp *dhcp.Manager
}

type admitv1beta1Func func(context.Context, v1beta1.AdmissionReview) *v1beta1.AdmissionResponse

type admitv1Func func(context.Context, v1.AdmissionReview) *v1.AdmissionResponse

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
	return func(ctx context.Context, review v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
		in := v1.AdmissionReview{Request: convertAdmissionRequestToV1(review.Request)}
		out := f(ctx, in)
		return convertAdmissionResponseToV1beta1(out)
	}
}

func serve(w http.ResponseWriter, r *http.Request, admit admitHandler) {
	ctx := r.Context()
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		plog.G(ctx).Errorf("ContentType=%s, expect application/json", contentType)
		return
	}

	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(body, nil, nil)
	if err != nil {
		msg := fmt.Sprintf("Request: %s could not be decoded: %v", string(body), err)
		plog.G(ctx).Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	var responseObj runtime.Object
	switch *gvk {
	case v1beta1.SchemeGroupVersion.WithKind("AdmissionReview"):
		review, ok := obj.(*v1beta1.AdmissionReview)
		if !ok {
			plog.G(ctx).Errorf("Expected v1beta1.AdmissionReview but got: %T", obj)
			return
		}
		if isDryRun(review.Request.DryRun) {
			responseObj = &v1beta1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind},
				Response: &v1beta1.AdmissionResponse{Allowed: true, UID: review.Request.UID},
			}
		} else {
			resp := &v1beta1.AdmissionReview{}
			resp.SetGroupVersionKind(*gvk)
			resp.Response = admit.v1beta1(ctx, *review)
			resp.Response.UID = review.Request.UID
			responseObj = resp
		}

	case v1.SchemeGroupVersion.WithKind("AdmissionReview"):
		review, ok := obj.(*v1.AdmissionReview)
		if !ok {
			plog.G(ctx).Errorf("Expected v1.AdmissionReview but got: %T", obj)
			return
		}
		if isDryRun(review.Request.DryRun) {
			responseObj = allowedReview(gvk, review.Request.UID)
		} else {
			resp := &v1.AdmissionReview{}
			resp.SetGroupVersionKind(*gvk)
			resp.Response = admit.v1(ctx, *review)
			resp.Response.UID = review.Request.UID
			responseObj = resp
		}

	default:
		msg := fmt.Sprintf("Unsupported group version kind: %v", gvk)
		plog.G(ctx).Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	respBytes, err := json.Marshal(responseObj)
	if err != nil {
		plog.G(ctx).Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(respBytes); err != nil {
		plog.G(ctx).Error(err)
	}
}

func isDryRun(dryRun *bool) bool {
	return ptr.Deref(dryRun, false)
}

func allowedReview(gvk *schema.GroupVersionKind, uid types.UID) *v1.AdmissionReview {
	return &v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
		},
		Response: &v1.AdmissionResponse{
			Allowed: true,
			UID:     uid,
		},
	}
}

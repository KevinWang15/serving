package utils

import (
	"encoding/json"
	"github.com/knative/serving/pkg/apis/serving"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func AddCustomDataFromRevisionSpecToServiceAnnotations(desiredService *corev1.Service, rev *v1alpha1.Revision) error {
	if desiredService.Annotations == nil {
		desiredService.Annotations = make(map[string]string)
	}
	customDataJSON, err := json.Marshal(rev.Spec.CustomData)
	if err != nil {
		return err
	}
	desiredService.Annotations[serving.CustomDataAnnotationKey] = string(customDataJSON)
	return err
}

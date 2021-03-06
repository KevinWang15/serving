/*
Copyright 2018 The Knative Authors

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

package autoscaling

import (
	"context"
	"sync"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/scale"

	"github.com/knative/pkg/apis"
	"github.com/knative/pkg/configmap"
	"github.com/knative/pkg/logging"
	kpa "github.com/knative/serving/pkg/apis/autoscaling/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/autoscaler"
	clientset "github.com/knative/serving/pkg/client/clientset/versioned"
)

const ScaleUnknown = -1

// kpaScaler scales the target of a KPA up or down including scaling to zero.
type kpaScaler struct {
	servingClientSet clientset.Interface
	scaleClientSet   scale.ScalesGetter
	logger           *zap.SugaredLogger

	// autoscalerConfig could change over time and access to it
	// must go through autoscalerConfigMutex
	autoscalerConfig      *autoscaler.Config
	autoscalerConfigMutex sync.Mutex
}

// NewKPAScaler creates a kpaScaler.
func NewKPAScaler(servingClientSet clientset.Interface, scaleClientSet scale.ScalesGetter,
	logger *zap.SugaredLogger, configMapWatcher configmap.Watcher) KPAScaler {
	ks := &kpaScaler{
		servingClientSet: servingClientSet,
		scaleClientSet:   scaleClientSet,
		logger:           logger,
	}

	// Watch for config changes.
	configMapWatcher.Watch(autoscaler.ConfigName, ks.receiveAutoscalerConfig)

	return ks
}

func (ks *kpaScaler) receiveAutoscalerConfig(configMap *corev1.ConfigMap) {
	newAutoscalerConfig, err := autoscaler.NewConfigFromConfigMap(configMap)
	ks.autoscalerConfigMutex.Lock()
	defer ks.autoscalerConfigMutex.Unlock()
	if err != nil {
		if ks.autoscalerConfig != nil {
			ks.logger.Errorf("Error updating Autoscaler ConfigMap: %v", err)
		} else {
			ks.logger.Fatalf("Error initializing Autoscaler ConfigMap: %v", err)
		}
		return
	}
	ks.logger.Infof("Autoscaler config map is added or updated: %v", configMap)
	ks.autoscalerConfig = newAutoscalerConfig
}

func (ks *kpaScaler) getAutoscalerConfig() *autoscaler.Config {
	ks.autoscalerConfigMutex.Lock()
	defer ks.autoscalerConfigMutex.Unlock()
	return ks.autoscalerConfig.DeepCopy()
}

// pre: 0 <= min <= max && 0 <= x
func applyBounds(min, max int32) func(int32) int32 {
	return func(x int32) int32 {
		if x < min {
			return min
		}
		if max != 0 && x > max {
			return max
		}
		return x
	}
}

// Scale attempts to scale the given KPA's target reference to the desired scale.
func (ks *kpaScaler) Scale(ctx context.Context, kpa *kpa.PodAutoscaler, desiredScale int32) (int32, error) {
	logger := logging.FromContext(ctx)

	// TODO(mattmoor): Drop this once the KPA is the source of truth and we
	// scale exclusively on metrics.
	revGVK := v1alpha1.SchemeGroupVersion.WithKind("Revision")
	owner := metav1.GetControllerOf(kpa)
	if owner == nil || owner.Kind != revGVK.Kind ||
		owner.APIVersion != revGVK.GroupVersion().String() {
		logger.Debug("KPA is not owned by a Revision.")
		return desiredScale, nil
	}

	gv, err := schema.ParseGroupVersion(kpa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		logger.Error("Unable to parse APIVersion.", zap.Error(err))
		return desiredScale, err
	}
	resource := apis.KindToResource(gv.WithKind(kpa.Spec.ScaleTargetRef.Kind)).GroupResource()
	resourceName := kpa.Spec.ScaleTargetRef.Name

	// Identify the current scale.
	scl, err := ks.scaleClientSet.Scales(kpa.Namespace).Get(resource, resourceName)
	if err != nil {
		logger.Errorf("Resource %q not found.", resourceName, zap.Error(err))
		return desiredScale, err
	}
	currentScale := scl.Spec.Replicas

	if desiredScale == 0 {
		// We should only scale to zero when both of the following conditions are true:
		//   a) The KPA has been active for atleast the idle period, after which it gets marked inactive
		//   b) The KPA has been inactive for atleast the grace period

		config := ks.getAutoscalerConfig()

		if kpa.Status.IsActivating() { // Active=Unknown
			// Don't scale-to-zero during activation
			desiredScale = ScaleUnknown
		} else if kpa.Status.IsReady() { // Active=True
			// Don't scale-to-zero if the KPA is active

			// If the idle period has elapsed, let the reconciler mark it inactive
			if kpa.Status.CanMarkInactive(config.ScaleToZeroIdlePeriod) {
				return desiredScale, nil
			}
			// Otherwise, scale down to 1 until the idle period elapses
			desiredScale = 1
		} else { // Active=False
			// Don't scale-to-zero if the grace period hasn't elapsed
			if !kpa.Status.CanScaleToZero(config.ScaleToZeroGracePeriod) {
				return desiredScale, nil
			}
		}
	}

	// Scale from zero. When there are no metrics scale to 1.
	if currentScale == 0 && desiredScale == -1 {
		logger.Debugf("Scaling up from 0 to 1")
		desiredScale = 1
	}

	if desiredScale < 0 {
		logger.Debug("Metrics are not yet being collected.")
		return desiredScale, nil
	}

	if newScale := applyBounds(kpa.ScaleBounds())(desiredScale); newScale != desiredScale {
		logger.Debugf("Adjusting desiredScale: %v -> %v", desiredScale, newScale)
		desiredScale = newScale
	}

	if desiredScale == currentScale {
		return desiredScale, nil
	}
	logger.Infof("Scaling from %d to %d", currentScale, desiredScale)

	// Scale the target reference.
	scl.Spec.Replicas = desiredScale
	_, err = ks.scaleClientSet.Scales(kpa.Namespace).Update(resource, scl)
	if err != nil {
		logger.Errorf("Error scaling target reference %v.", resourceName, zap.Error(err))
		return desiredScale, err
	}

	logger.Debug("Successfully scaled.")
	return desiredScale, nil
}

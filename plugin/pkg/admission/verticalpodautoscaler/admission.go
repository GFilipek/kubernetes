/*
Copyright 2015 The Kubernetes Authors.

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

package verticalpodautoscaler

import (
	"flag"
	"github.com/golang/glog"
	"io"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/plugin/pkg/admission/verticalpodautoscaler/apimock"
	"k8s.io/kubernetes/plugin/pkg/admission/verticalpodautoscaler/recommender"
)

// WARNING: this feature is experimental and will definitely change.
func init() {
	admission.RegisterPlugin("VerticalPodAutoscaler", func(config io.Reader) (admission.Interface, error) {
		return newVerticalPodAutoscaler(), nil
	})
}

type verticalPodAutoscaler struct {
	*admission.Handler
	vpaLister   apimock.VerticalPodAutoscalerLister
	chachingRec recommender.CachingRecommender
}

func newVerticalPodAutoscaler() admission.Interface {
	return &verticalPodAutoscaler{
		Handler: admission.NewHandler(admission.Create),
	}
}

func (vpa verticalPodAutoscaler) Admit(a admission.Attributes) (err error) {
	// Ignore all calls to subresources or resources other than pods.
	if a.GetSubresource() != "" || a.GetResource().GroupResource() != api.Resource("pods") {
		return nil
	}
	pod, ok := a.GetObject().(*api.Pod)
	if !ok {
		return apierrors.NewBadRequest("resource was marked with kind : Pod, but was unable to be converted")
	}

	vpaConfig := vpa.getMatchingVPA(pod)
	if vpaConfig == nil {
		return nil
	}

	recommendation, err := vpa.chachingRec.Get(&pod.Spec)
	if err != nil || recommendation == nil {
		if vpaConfig.Status.Recommendation != nil {
			// fallback to recommendation cached in VPA config
			recommendation = vpaConfig.Status.Recommendation
		} else {
			// no recommendation to apply
			return nil
		}
	}

	applyRecomendedResources(pod, recommendation, vpaConfig.Spec.ResourcesPolicy)
	return nil
}

// applyRecomendedResources overwrites pod resources Request field with recommended values
func applyRecomendedResources(pod *api.Pod, recommendation *apimock.Recommendation, policy apimock.ResourcesPolicy) {
	for _, container := range pod.Spec.Containers {
		containerRecommendation := getRecommendationForContainer(recommendation, container)
		if containerRecommendation == nil {
			continue
		}
		containerPolicy := getContainerPolicy(container.Name, &policy)
		updateRecommendationLimits(containerRecommendation, containerPolicy)
		for resource, recommended := range containerRecommendation.Resources {
			requested, exists := container.Resources.Requests[resource]
			if exists {
				// overwriting existing resource spec
				glog.V(2).Infof("updating resources request for pod %v container %v resource %v old value %v new value %v",
					pod.Name, container.Name, resource, requested, recommended)
			} else {
				// adding new resource spec
				glog.V(2).Infof("updated resources request for pod %v container %v resource %v old value missing new value %v",
					pod.Name, container.Name, resource, recommended)
			}
			container.Resources.Requests[resource] = recommended
			// TODO: container.Resources.Limits should be updated?
		}
	}
}

// updates recommendation if recommended resources exceed limits defined in VPA resources policy
func updateRecommendationLimits(recommendation *apimock.ContainerRecommendation, policy *apimock.ContainerPolicy) {
	for resourceName, recommended := range recommendation.Resources {
		if policy != nil {
			resourcePolicy, found := policy.ResourcePolicy[resourceName]
			if found {
				if !resourcePolicy.Min.IsZero() && recommended.Value() < resourcePolicy.Min.Value() {
					glog.Warningf("recommendation outside of policy bounds : min value : %v recommended : %v",
						resourcePolicy.Min.Value(), recommended)
					recommendation.Resources[resourceName] = resourcePolicy.Min
				}
				if !resourcePolicy.Max.IsZero() && recommended.Value() > resourcePolicy.Max.Value() {
					glog.Warningf("recommendation outside of policy bounds : max value : %v recommended : %v",
						resourcePolicy.Max.Value(), recommended)
					recommendation.Resources[resourceName] = resourcePolicy.Max
				}
			}
		}

	}
}

// This should cached as part of vpaLister
func (vpa *verticalPodAutoscaler) getMatchingVPA(pod *api.Pod) *apimock.VerticalPodAutoscaler {
	configs, err := vpa.vpaLister.List()
	if err != nil {
		//cannot get vpa configs
		return nil
	}
	for _, vpaConfig := range configs {
		selector, err := labels.Parse(vpaConfig.Spec.Target.Selector)
		if err != nil {
			continue
		}
		if selector.Matches(labels.Set(pod.GetLabels())) {
			return vpaConfig
		}
	}
	return nil
}

func getRecommendationForContainer(recommendation *apimock.Recommendation, container api.Container) *apimock.ContainerRecommendation {
	for i, containerRec := range recommendation.Containers {
		if containerRec.Name == container.Name {
			return &recommendation.Containers[i]
		}
	}
	return nil
}

func getContainerPolicy(containerName string, policy *apimock.ResourcesPolicy) *apimock.ContainerPolicy {
	if policy != nil {
		for _, container := range policy.Containers {
			if containerName == container.Name {
				return &container
			}
		}
	}
	return nil
}

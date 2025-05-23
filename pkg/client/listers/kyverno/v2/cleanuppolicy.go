/*
Copyright The Kubernetes Authors.

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

// Code generated by lister-gen. DO NOT EDIT.

package v2

import (
	kyvernov2 "github.com/kyverno/kyverno/api/kyverno/v2"
	labels "k8s.io/apimachinery/pkg/labels"
	listers "k8s.io/client-go/listers"
	cache "k8s.io/client-go/tools/cache"
)

// CleanupPolicyLister helps list CleanupPolicies.
// All objects returned here must be treated as read-only.
type CleanupPolicyLister interface {
	// List lists all CleanupPolicies in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*kyvernov2.CleanupPolicy, err error)
	// CleanupPolicies returns an object that can list and get CleanupPolicies.
	CleanupPolicies(namespace string) CleanupPolicyNamespaceLister
	CleanupPolicyListerExpansion
}

// cleanupPolicyLister implements the CleanupPolicyLister interface.
type cleanupPolicyLister struct {
	listers.ResourceIndexer[*kyvernov2.CleanupPolicy]
}

// NewCleanupPolicyLister returns a new CleanupPolicyLister.
func NewCleanupPolicyLister(indexer cache.Indexer) CleanupPolicyLister {
	return &cleanupPolicyLister{listers.New[*kyvernov2.CleanupPolicy](indexer, kyvernov2.Resource("cleanuppolicy"))}
}

// CleanupPolicies returns an object that can list and get CleanupPolicies.
func (s *cleanupPolicyLister) CleanupPolicies(namespace string) CleanupPolicyNamespaceLister {
	return cleanupPolicyNamespaceLister{listers.NewNamespaced[*kyvernov2.CleanupPolicy](s.ResourceIndexer, namespace)}
}

// CleanupPolicyNamespaceLister helps list and get CleanupPolicies.
// All objects returned here must be treated as read-only.
type CleanupPolicyNamespaceLister interface {
	// List lists all CleanupPolicies in the indexer for a given namespace.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*kyvernov2.CleanupPolicy, err error)
	// Get retrieves the CleanupPolicy from the indexer for a given namespace and name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*kyvernov2.CleanupPolicy, error)
	CleanupPolicyNamespaceListerExpansion
}

// cleanupPolicyNamespaceLister implements the CleanupPolicyNamespaceLister
// interface.
type cleanupPolicyNamespaceLister struct {
	listers.ResourceIndexer[*kyvernov2.CleanupPolicy]
}

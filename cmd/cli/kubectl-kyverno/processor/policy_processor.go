package processor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	json_patch "github.com/evanphx/json-patch/v5"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov2 "github.com/kyverno/kyverno/api/kyverno/v2"
	policiesv1alpha1 "github.com/kyverno/kyverno/api/policies.kyverno.io/v1alpha1"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/apis/v1alpha1"
	clicontext "github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/context"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/data"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/log"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/store"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/utils/common"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/variables"
	"github.com/kyverno/kyverno/pkg/admissionpolicy"
	celengine "github.com/kyverno/kyverno/pkg/cel/engine"
	"github.com/kyverno/kyverno/pkg/cel/matching"
	celpolicy "github.com/kyverno/kyverno/pkg/cel/policy"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/engine"
	"github.com/kyverno/kyverno/pkg/engine/adapters"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	"github.com/kyverno/kyverno/pkg/engine/factories"
	"github.com/kyverno/kyverno/pkg/engine/jmespath"
	"github.com/kyverno/kyverno/pkg/engine/mutate/patch"
	"github.com/kyverno/kyverno/pkg/engine/policycontext"
	"github.com/kyverno/kyverno/pkg/exceptions"
	gctxstore "github.com/kyverno/kyverno/pkg/globalcontext/store"
	"github.com/kyverno/kyverno/pkg/imageverification/imagedataloader"
	"github.com/kyverno/kyverno/pkg/imageverifycache"
	"github.com/kyverno/kyverno/pkg/registryclient"
	jsonutils "github.com/kyverno/kyverno/pkg/utils/json"
	"gomodules.xyz/jsonpatch/v2"
	yamlv2 "gopkg.in/yaml.v2"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/restmapper"
)

type PolicyProcessor struct {
	Store                             *store.Store
	Policies                          []kyvernov1.PolicyInterface
	ValidatingAdmissionPolicies       []admissionregistrationv1.ValidatingAdmissionPolicy
	ValidatingAdmissionPolicyBindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding
	ValidatingPolicies                []policiesv1alpha1.ValidatingPolicy
	Resource                          unstructured.Unstructured
	JsonPayload                       unstructured.Unstructured
	PolicyExceptions                  []*kyvernov2.PolicyException
	CELExceptions                     []*policiesv1alpha1.PolicyException
	MutateLogPath                     string
	MutateLogPathIsDir                bool
	Variables                         *variables.Variables
	// TODO
	ContextPath               string
	Cluster                   bool
	UserInfo                  *kyvernov2.RequestInfo
	PolicyReport              bool
	NamespaceSelectorMap      map[string]map[string]string
	Stdin                     bool
	Rc                        *ResultCounts
	PrintPatchResource        bool
	RuleToCloneSourceResource map[string]string
	Client                    dclient.Interface
	AuditWarn                 bool
	Subresources              []v1alpha1.Subresource
	Out                       io.Writer
}

func (p *PolicyProcessor) ApplyPoliciesOnResource() ([]engineapi.EngineResponse, error) {
	cfg := config.NewDefaultConfiguration(false)
	jp := jmespath.New(cfg)
	resource := p.Resource
	namespaceLabels := p.NamespaceSelectorMap[p.Resource.GetNamespace()]
	policyExceptionLister := &policyExceptionLister{
		exceptions: p.PolicyExceptions,
	}
	var client engineapi.Client
	if p.Client != nil {
		client = adapters.Client(p.Client)
	}
	rclient := p.Store.GetRegistryClient()
	if rclient == nil {
		rclient = registryclient.NewOrDie()
	}
	isCluster := false
	eng := engine.NewEngine(
		cfg,
		config.NewDefaultMetricsConfiguration(),
		jmespath.New(cfg),
		client,
		factories.DefaultRegistryClientFactory(adapters.RegistryClient(rclient), nil),
		imageverifycache.DisabledImageVerifyCache(),
		store.ContextLoaderFactory(p.Store, nil),
		exceptions.New(policyExceptionLister),
		&isCluster,
	)
	gvk, subresource := resource.GroupVersionKind(), ""
	resourceKind := resource.GetKind()
	resourceName := resource.GetName()
	resourceNamespace := resource.GetNamespace()
	// If --cluster flag is not set, then we need to find the top level resource GVK and subresource
	if !p.Cluster {
		for _, s := range p.Subresources {
			subgvk := schema.GroupVersionKind{
				Group:   s.Subresource.Group,
				Version: s.Subresource.Version,
				Kind:    s.Subresource.Kind,
			}
			if gvk == subgvk {
				gvk = schema.GroupVersionKind{
					Group:   s.ParentResource.Group,
					Version: s.ParentResource.Version,
					Kind:    s.ParentResource.Kind,
				}
				parts := strings.Split(s.Subresource.Name, "/")
				subresource = parts[1]
			}
		}
	} else {
		if len(namespaceLabels) == 0 && resourceKind != "Namespace" && resourceNamespace != "" {
			ns, err := p.Client.GetResource(context.TODO(), "v1", "Namespace", "", resourceNamespace)
			if err != nil {
				log.Log.Error(err, "failed to get the resource's namespace")
				return nil, fmt.Errorf("failed to get the resource's namespace (%w)", err)
			}
			namespaceLabels = ns.GetLabels()
		}
	}
	resPath := fmt.Sprintf("%s/%s/%s", resourceNamespace, resourceKind, resourceName)
	responses := make([]engineapi.EngineResponse, 0, len(p.Policies))
	// mutate
	for _, policy := range p.Policies {
		if !policy.GetSpec().HasMutate() {
			continue
		}
		policyContext, err := p.makePolicyContext(jp, cfg, resource, policy, namespaceLabels, gvk, subresource)
		if err != nil {
			return responses, err
		}
		mutateResponse := eng.Mutate(context.Background(), policyContext)
		err = p.processMutateEngineResponse(mutateResponse, resPath)
		if err != nil {
			return responses, fmt.Errorf("failed to print mutated result (%w)", err)
		}
		responses = append(responses, mutateResponse)
		resource = mutateResponse.PatchedResource
	}
	// verify images
	for _, policy := range p.Policies {
		if !policy.GetSpec().HasVerifyImages() {
			continue
		}
		policyContext, err := p.makePolicyContext(jp, cfg, resource, policy, namespaceLabels, gvk, subresource)
		if err != nil {
			return responses, err
		}
		verifyImageResponse, verifiedImageData := eng.VerifyAndPatchImages(context.TODO(), policyContext)
		// update annotation to reflect verified images
		var patches []jsonpatch.JsonPatchOperation
		if !verifiedImageData.IsEmpty() {
			annotationPatches, err := verifiedImageData.Patches(len(verifyImageResponse.PatchedResource.GetAnnotations()) != 0, log.Log)
			if err != nil {
				return responses, err
			}
			// add annotation patches first
			patches = append(annotationPatches, patches...)
		}
		if len(patches) != 0 {
			patch := jsonutils.JoinPatches(patch.ConvertPatches(patches...)...)
			decoded, err := json_patch.DecodePatch(patch)
			if err != nil {
				return responses, err
			}
			options := &json_patch.ApplyOptions{SupportNegativeIndices: true, AllowMissingPathOnRemove: true, EnsurePathExistsOnAdd: true}
			resourceBytes, err := verifyImageResponse.PatchedResource.MarshalJSON()
			if err != nil {
				return responses, err
			}
			patchedResourceBytes, err := decoded.ApplyWithOptions(resourceBytes, options)
			if err != nil {
				return responses, err
			}
			if err := verifyImageResponse.PatchedResource.UnmarshalJSON(patchedResourceBytes); err != nil {
				return responses, err
			}
		}
		responses = append(responses, verifyImageResponse)
		resource = verifyImageResponse.PatchedResource
	}
	// validate
	for _, policy := range p.Policies {
		if !policyHasValidateOrVerifyImageChecks(policy) {
			continue
		}
		policyContext, err := p.makePolicyContext(jp, cfg, resource, policy, namespaceLabels, gvk, subresource)
		if err != nil {
			return responses, err
		}
		validateResponse := eng.Validate(context.TODO(), policyContext)
		responses = append(responses, validateResponse)
		resource = validateResponse.PatchedResource
	}
	// validating admission policies
	vapResponses := make([]engineapi.EngineResponse, 0, len(p.ValidatingAdmissionPolicies))
	for _, policy := range p.ValidatingAdmissionPolicies {
		policyData := admissionpolicy.NewPolicyData(policy)
		for _, binding := range p.ValidatingAdmissionPolicyBindings {
			if binding.Spec.PolicyName == policy.Name {
				policyData.AddBinding(binding)
			}
		}
		validateResponse, _ := admissionpolicy.Validate(policyData, resource, p.NamespaceSelectorMap, p.Client)
		vapResponses = append(vapResponses, validateResponse)
		p.Rc.addValidatingAdmissionResponse(validateResponse)
	}
	// validating admission policies
	if len(p.ValidatingPolicies) != 0 {
		ctx := context.TODO()
		compiler := celpolicy.NewCompiler()
		provider, err := celengine.NewProvider(compiler, p.ValidatingPolicies, p.CELExceptions)
		if err != nil {
			return nil, err
		}
		// TODO: mock when no cluster provided
		gctxStore := gctxstore.New()
		var restMapper meta.RESTMapper
		var contextProvider celpolicy.Context
		if p.Client != nil {
			contextProvider, err = celpolicy.NewContextProvider(
				p.Client,
				// TODO
				[]imagedataloader.Option{imagedataloader.WithLocalCredentials(true)},
				gctxStore,
			)
			if err != nil {
				return nil, err
			}
			apiGroupResources, err := restmapper.GetAPIGroupResources(p.Client.GetKubeClient().Discovery())
			if err != nil {
				return nil, err
			}
			restMapper = restmapper.NewDiscoveryRESTMapper(apiGroupResources)
		} else {
			apiGroupResources, err := data.APIGroupResources()
			if err != nil {
				return nil, err
			}
			restMapper = restmapper.NewDiscoveryRESTMapper(apiGroupResources)
			fakeContextProvider := celpolicy.NewFakeContextProvider()
			if p.ContextPath != "" {
				ctx, err := clicontext.Load(nil, p.ContextPath)
				if err != nil {
					return nil, err
				}
				for _, resource := range ctx.ContextSpec.Resources {
					gvk := resource.GroupVersionKind()
					mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
					if err != nil {
						return nil, err
					}
					if err := fakeContextProvider.AddResource(mapping.Resource, &resource); err != nil {
						return nil, err
					}
				}
			}
			contextProvider = fakeContextProvider
		}
		if p.Resource.Object != nil {
			eng := celengine.NewEngine(provider, p.Variables.Namespace, matching.NewMatcher())
			// get gvk from resource
			gvk := resource.GroupVersionKind()
			// map gvk to gvr
			mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				return nil, fmt.Errorf("failed to map gvk to gvr %s (%v)\n", gvk, err)
			}
			gvr := mapping.Resource
			var user authenticationv1.UserInfo
			if p.UserInfo != nil {
				user = p.UserInfo.AdmissionUserInfo
			}
			// create engine request
			request := celengine.Request(
				contextProvider,
				gvk,
				gvr,
				// TODO: how to manage subresource ?
				"",
				resource.GetName(),
				resource.GetNamespace(),
				// TODO: how to manage other operations ?
				admissionv1.Create,
				user,
				&resource,
				nil,
				false,
				nil,
			)
			reps, err := eng.Handle(ctx, request)
			if err != nil {
				return nil, fmt.Errorf("failed to apply validating policies on resource %s (%w)", resource.GetName(), err)
			}
			for _, r := range reps.Policies {
				response := engineapi.EngineResponse{
					Resource: *reps.Resource,
					PolicyResponse: engineapi.PolicyResponse{
						Rules: r.Rules,
					},
				}
				response = response.WithPolicy(engineapi.NewValidatingPolicy(&r.Policy))
				responses = append(responses, response)
			}
		}
		if p.JsonPayload.Object != nil {
			eng := celengine.NewEngine(provider, nil, nil)
			request := celengine.RequestFromJSON(contextProvider, &unstructured.Unstructured{Object: p.JsonPayload.Object})
			reps, err := eng.Handle(ctx, request)
			if err != nil {
				return nil, err
			}
			for _, r := range reps.Policies {
				response := engineapi.EngineResponse{
					Resource: *reps.Resource,
					PolicyResponse: engineapi.PolicyResponse{
						Rules: r.Rules,
					},
				}
				response = response.WithPolicy(engineapi.NewValidatingPolicy(&r.Policy))
				responses = append(responses, response)
			}
		}
	}
	// generate
	for _, policy := range p.Policies {
		if policy.GetSpec().HasGenerate() {
			policyContext, err := p.makePolicyContext(jp, cfg, resource, policy, namespaceLabels, gvk, subresource)
			if err != nil {
				return responses, err
			}
			generateResponse := eng.ApplyBackgroundChecks(context.TODO(), policyContext)
			if !generateResponse.IsEmpty() {
				newRuleResponse, err := handleGeneratePolicy(p.Out, p.Store, &generateResponse, *policyContext, p.RuleToCloneSourceResource)
				if err != nil {
					log.Log.Error(err, "failed to apply generate policy")
				} else {
					generateResponse.PolicyResponse.Rules = newRuleResponse
				}
				if err := p.processGenerateResponse(generateResponse, resPath); err != nil {
					return responses, err
				}
				responses = append(responses, generateResponse)
			}
			p.Rc.addGenerateResponse(generateResponse)
		}
	}
	p.Rc.addEngineResponses(p.AuditWarn, responses...)
	responses = append(responses, vapResponses...)
	return responses, nil
}

func (p *PolicyProcessor) makePolicyContext(
	jp jmespath.Interface,
	cfg config.Configuration,
	resource unstructured.Unstructured,
	policy kyvernov1.PolicyInterface,
	namespaceLabels map[string]string,
	gvk schema.GroupVersionKind,
	subresource string,
) (*policycontext.PolicyContext, error) {
	operation := kyvernov1.Create
	var resourceValues map[string]interface{}
	if p.Variables != nil {
		kindOnwhichPolicyIsApplied := common.GetKindsFromPolicy(p.Out, policy, p.Variables.Subresources(), p.Client)
		vals, err := p.Variables.ComputeVariables(p.Store, policy.GetName(), resource.GetName(), resource.GetKind(), kindOnwhichPolicyIsApplied /*matches...*/)
		if err != nil {
			return nil, fmt.Errorf("policy `%s` have variables. pass the values for the variables for resource `%s` using set/values_file flag (%w)",
				policy.GetName(),
				resource.GetName(),
				err,
			)
		}
		resourceValues = vals
	}
	// TODO: this is kind of buggy, we should read that from the json context
	switch resourceValues["request.operation"] {
	case "DELETE":
		operation = kyvernov1.Delete
	case "UPDATE":
		operation = kyvernov1.Update
	}
	policyContext, err := engine.NewPolicyContext(
		jp,
		resource,
		operation,
		p.UserInfo,
		cfg,
	)
	if err != nil {
		log.Log.Error(err, "failed to create policy context")
		return nil, fmt.Errorf("failed to create policy context (%w)", err)
	}
	if operation == kyvernov1.Update {
		resource := resource.DeepCopy()
		policyContext = policyContext.WithOldResource(*resource)
		if err := policyContext.JSONContext().AddOldResource(resource.Object); err != nil {
			return nil, fmt.Errorf("failed to update old resource in json context (%w)", err)
		}
	}
	if operation == kyvernov1.Delete {
		policyContext = policyContext.WithOldResource(resource)
		if err := policyContext.JSONContext().AddOldResource(resource.Object); err != nil {
			return nil, fmt.Errorf("failed to update old resource in json context (%w)", err)
		}
	}
	policyContext = policyContext.
		WithPolicy(policy).
		WithNamespaceLabels(namespaceLabels).
		WithResourceKind(gvk, subresource)
	for key, value := range resourceValues {
		err = policyContext.JSONContext().AddVariable(key, value)
		if err != nil {
			log.Log.Error(err, "failed to add variable to context", "key", key, "value", value)
			return nil, fmt.Errorf("failed to add variable to context %s (%w)", key, err)
		}
	}
	// we need to get the resources back from the context to account for injected variables
	switch operation {
	case kyvernov1.Create:
		ret, err := policyContext.JSONContext().Query("request.object")
		if err != nil {
			return nil, err
		}
		if ret == nil {
			policyContext = policyContext.WithNewResource(unstructured.Unstructured{})
		} else {
			object, ok := ret.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("the object retrieved from the json context is not valid")
			}
			policyContext = policyContext.WithNewResource(unstructured.Unstructured{Object: object})
		}
	case kyvernov1.Update:
		{
			ret, err := policyContext.JSONContext().Query("request.object")
			if err != nil {
				return nil, err
			}
			if ret == nil {
				policyContext = policyContext.WithNewResource(unstructured.Unstructured{})
			} else {
				object, ok := ret.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("the object retrieved from the json context is not valid")
				}
				policyContext = policyContext.WithNewResource(unstructured.Unstructured{Object: object})
			}
		}
		{
			ret, err := policyContext.JSONContext().Query("request.oldObject")
			if err != nil {
				return nil, err
			}
			if ret == nil {
				policyContext = policyContext.WithOldResource(unstructured.Unstructured{})
			} else {
				object, ok := ret.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("the object retrieved from the json context is not valid")
				}
				policyContext = policyContext.WithOldResource(unstructured.Unstructured{Object: object})
			}
		}
	case kyvernov1.Delete:
		ret, err := policyContext.JSONContext().Query("request.oldObject")
		if err != nil {
			return nil, err
		}
		if ret == nil {
			policyContext = policyContext.WithOldResource(unstructured.Unstructured{})
		} else {
			object, ok := ret.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("the object retrieved from the json context is not valid")
			}
			policyContext = policyContext.WithOldResource(unstructured.Unstructured{Object: object})
		}
	}
	return policyContext, nil
}

func (p *PolicyProcessor) processGenerateResponse(response engineapi.EngineResponse, resourcePath string) error {
	generatedResources := []*unstructured.Unstructured{}
	for _, rule := range response.PolicyResponse.Rules {
		gen := rule.GeneratedResources()
		generatedResources = append(generatedResources, gen...)
	}
	for _, r := range generatedResources {
		err := p.printOutput(r.Object, response, resourcePath, true)
		if err != nil {
			return fmt.Errorf("failed to print generate result (%w)", err)
		}
		fmt.Fprintf(p.Out, "\n\nGenerate:\nGeneration completed successfully.")
	}
	return nil
}

func (p *PolicyProcessor) processMutateEngineResponse(response engineapi.EngineResponse, resourcePath string) error {
	p.Rc.addMutateResponse(response)
	err := p.printOutput(response.PatchedResource.Object, response, resourcePath, false)
	if err != nil {
		return fmt.Errorf("failed to print mutated result (%w)", err)
	}
	fmt.Fprintf(p.Out, "\n\nMutation:\nMutation has been applied successfully.")
	return nil
}

func (p *PolicyProcessor) printOutput(resource interface{}, response engineapi.EngineResponse, resourcePath string, isGenerate bool) error {
	yamlEncodedResource, err := yamlv2.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to marshal (%w)", err)
	}

	var yamlEncodedTargetResources [][]byte
	for _, ruleResponese := range response.PolicyResponse.Rules {
		patchedTarget, _, _ := ruleResponese.PatchedTarget()

		if patchedTarget != nil {
			yamlEncodedResource, err := yamlv2.Marshal(patchedTarget.Object)
			if err != nil {
				return fmt.Errorf("failed to marshal (%w)", err)
			}

			yamlEncodedResource = append(yamlEncodedResource, []byte("\n---\n")...)
			yamlEncodedTargetResources = append(yamlEncodedTargetResources, yamlEncodedResource)
		}
	}

	if p.MutateLogPath == "" {
		resource := string(yamlEncodedResource) + string("\n---")
		if len(strings.TrimSpace(resource)) > 0 {
			if !p.Stdin {
				fmt.Fprintf(p.Out, "\npolicy %s applied to %s:", response.Policy().GetName(), resourcePath)
			}
			fmt.Fprintf(p.Out, "\n"+resource+"\n") //nolint:govet
			if len(yamlEncodedTargetResources) > 0 {
				fmt.Fprintf(p.Out, "patched targets: \n")
				for _, patchedTarget := range yamlEncodedTargetResources {
					fmt.Fprintf(p.Out, "\n"+string(patchedTarget)+"\n")
				}
			}
		}
		return nil
	}

	var file *os.File
	mutateLogPath := filepath.Clean(p.MutateLogPath)
	filename := p.Resource.GetName() + "-mutated"
	if isGenerate {
		filename = response.Policy().GetName() + "-generated"
	}

	file, err = os.OpenFile(filepath.Join(mutateLogPath, filename+".yaml"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304
	if err != nil {
		return err
	}

	if !p.MutateLogPathIsDir {
		// truncation for the case when mutateLogPath is a file (not a directory) is handled under pkg/kyverno/apply/test_command.go
		f, err := os.OpenFile(mutateLogPath, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304
		if err != nil {
			return err
		}
		file = f
	}
	if _, err := file.Write([]byte(string(yamlEncodedResource) + "\n---\n\n")); err != nil {
		return err
	}

	for _, patchedTarget := range yamlEncodedTargetResources {
		if _, err := file.Write(patchedTarget); err != nil {
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

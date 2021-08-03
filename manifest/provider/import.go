package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-provider-kubernetes/manifest/morph"
	"github.com/hashicorp/terraform-provider-kubernetes/manifest/payload"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ImportResourceState function
func (s *RawProviderServer) ImportResourceState(ctx context.Context, req *tfprotov5.ImportResourceStateRequest) (*tfprotov5.ImportResourceStateResponse, error) {
	// Terraform only gives us the schema name of the resource and an ID string, as passed by the user on the command line.
	// The ID should be a combination of a Kubernetes GVK and a namespace/name type of resource identifier.
	// Without the user supplying the GRV there is no way to fully identify the resource when making the Get API call to K8s.
	// Presumably the Kubernetes API machinery already has a standard for expressing such a group. We should look there first.
	resp := &tfprotov5.ImportResourceStateResponse{}
	gvk, name, namespace, err := parseImportID(req.ID)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to parse import ID",
			Detail:   err.Error(),
		})
	}
	s.logger.Trace("[ImportResourceState]", "[ID]", gvk, name, namespace)
	rt, err := GetResourceType(req.TypeName)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to determine resource type",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	rm, err := s.getRestMapper()
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to get RESTMapper client",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	client, err := s.getDynamicClient()
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "failed to get Dynamic client",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	ns, err := IsResourceNamespaced(gvk, rm)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to get namespacing requirement from RESTMapper",
			Detail:   err.Error(),
		})
		return resp, nil
	}

	io := unstructured.Unstructured{}
	io.SetKind(gvk.Kind)
	io.SetAPIVersion(gvk.GroupVersion().String())
	io.SetName(name)
	io.SetNamespace(namespace)

	gvr, err := GVRFromUnstructured(&io, rm)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to get GVR from GVK via RESTMapper",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	rcl := client.Resource(gvr)

	var ro *unstructured.Unstructured
	if ns {
		ro, err = rcl.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	} else {
		ro, err = rcl.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  fmt.Sprintf("Failed to get resource %s from API", spew.Sdump(io)),
			Detail:   err.Error(),
		})
		return resp, nil
	}
	s.logger.Trace("[ImportResourceState]", "[API Resource]", spew.Sdump(ro))

	objectType, err := s.TFTypeFromOpenAPI(ctx, gvk, false)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  fmt.Sprintf("Failed to determine resource type from GVK: %s", gvk),
			Detail:   err.Error(),
		})
		return resp, nil
	}

	fo := RemoveServerSideFields(ro.UnstructuredContent())
	nobj, err := payload.ToTFValue(fo, objectType, tftypes.NewAttributePath())
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to convert unstructured to tftypes.Value",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	nobj, err = morph.DeepUnknown(objectType, nobj, tftypes.NewAttributePath())
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to backfill unknown values during import",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	s.logger.Trace("[ImportResourceState]", "[tftypes.Value]", spew.Sdump(nobj))

	newState := make(map[string]tftypes.Value)
	wftype := rt.(tftypes.Object).AttributeTypes["wait_for"]
	newState["manifest"] = tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}, nil)
	newState["object"] = morph.UnknownToNull(nobj)
	newState["wait_for"] = tftypes.NewValue(wftype, nil)
	nsVal := tftypes.NewValue(rt, newState)

	impState, err := tfprotov5.NewDynamicValue(nsVal.Type(), nsVal)
	if err != nil {
		resp.Diagnostics = append(resp.Diagnostics, &tfprotov5.Diagnostic{
			Severity: tfprotov5.DiagnosticSeverityError,
			Summary:  "Failed to construct dynamic value for imported state",
			Detail:   err.Error(),
		})
		return resp, nil
	}
	resp.ImportedResources = append(resp.ImportedResources, &tfprotov5.ImportedResource{
		TypeName: req.TypeName,
		State:    &impState,
	})
	return resp, nil
}

// parseImportID processes the resource ID string passed by the user to the "terraform import" command
// and extracts the values for GVK, name and (optionally) namespace of the target resource as required
// during the import process.
//
// The expected format for the import resource ID is:
//
// "<apiGroup/><apiVersion>#<Kind>#<namespace>#<name>"
//
// where 'namespace' is only required for resources that expect a namespace.
//
// Note the '#' separator between the elements of the ID string.
//
// Example: "v1#Secret#default#default-token-qgm6s"
//
func parseImportID(id string) (gvk schema.GroupVersionKind, name string, namespace string, err error) {
	parts := strings.Split(id, "#")
	if len(parts) < 3 || len(parts) > 4 {
		err = fmt.Errorf("invalid format for import ID [%s]", id)
		return
	}
	gvk = schema.FromAPIVersionAndKind(parts[0], parts[1])
	if len(parts) == 4 {
		namespace = parts[2]
		name = parts[3]
	} else {
		name = parts[2]
	}
	return
}
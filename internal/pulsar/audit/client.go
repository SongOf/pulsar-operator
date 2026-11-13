package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Client audits actual writes, including applier writes and PVC expansion or
// removal. Reads and unchanged status observations do not create audit rows.
type Client struct {
	client.Client
	Reader client.Reader
	Store  *Store
}

func (c *Client) Resource(obj client.Object) Resource {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if gvk, err := apiutil.GVKForObject(obj, c.Scheme()); err == nil {
		kind = gvk.Kind
	}
	return Resource{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()), Version: obj.GetResourceVersion(), Generation: obj.GetGeneration()}
}

func (c *Client) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.perform(ctx, "resource.create", obj, nil, false, func() error { return c.Client.Create(ctx, obj, opts...) })
}
func (c *Client) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.perform(ctx, "resource.update", obj, nil, false, func() error { return c.Client.Update(ctx, obj, opts...) })
}
func (c *Client) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.perform(ctx, "resource.patch", obj, patch, false, func() error { return c.Client.Patch(ctx, obj, patch, opts...) })
}
func (c *Client) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.perform(ctx, "resource.delete", obj, nil, false, func() error { return c.Client.Delete(ctx, obj, opts...) })
}
func (c *Client) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	return errors.New("audited deletes must identify individual resources")
}

func (c *Client) Status() client.SubResourceWriter { return c.SubResource("status") }
func (c *Client) SubResource(name string) client.SubResourceClient {
	delegate := c.Client.SubResource(name)
	if name != "status" {
		return &readOnlySubresource{SubResourceClient: delegate}
	}
	return &statusClient{SubResourceClient: delegate, parent: c}
}

type readOnlySubresource struct{ client.SubResourceClient }

func (*readOnlySubresource) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return errors.New("subresource write requires an explicit audit implementation")
}
func (*readOnlySubresource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("subresource write requires an explicit audit implementation")
}
func (*readOnlySubresource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return errors.New("subresource write requires an explicit audit implementation")
}

type statusClient struct {
	client.SubResourceClient
	parent *Client
}

func (c *statusClient) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return c.parent.perform(ctx, "status.update", obj, nil, true, func() error { return c.SubResourceClient.Update(ctx, obj, opts...) })
}
func (c *statusClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return c.parent.perform(ctx, "status.patch", obj, patch, true, func() error { return c.SubResourceClient.Patch(ctx, obj, patch, opts...) })
}

func (c *Client) perform(ctx context.Context, action string, obj client.Object, patch client.Patch, statusOnly bool, write func() error) error {
	if c.Store == nil {
		return errors.New("audit store is required for writes")
	}
	var before []byte
	var readErr error
	target := c.Resource(obj)
	expectedVersion := target.Version
	var observed Resource
	if action != "resource.create" {
		old := obj.DeepCopyObject().(client.Object)
		reader := c.Reader
		if reader == nil {
			reader = c.Client
		}
		readErr = reader.Get(ctx, client.ObjectKeyFromObject(obj), old)
		if readErr == nil {
			observed = c.Resource(old)
			if target.UID == "" {
				target = observed
			}
			before, readErr = json.Marshal(old)
		}
	}
	planned, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if action == "resource.delete" {
		planned = nil
	}
	knownIntent := true
	if patch != nil && readErr == nil {
		data, err := patch.Data(obj)
		if err != nil {
			return err
		}
		switch patch.Type() {
		case types.MergePatchType:
			planned, err = jsonpatch.MergePatch(before, data)
		case types.JSONPatchType:
			var decoded jsonpatch.Patch
			decoded, err = jsonpatch.DecodePatch(data)
			if err == nil {
				planned, err = decoded.Apply(before)
			}
		default:
			// The result snapshot is still audited; do not interpret strategic
			// merge or server-side apply as a JSON merge patch.
			planned = before
			knownIntent = false
		}
		if err != nil {
			planned = before
			knownIntent = false
		} // API admission records the actual failure.
	}
	changes, truncated := Diff(before, planned, statusOnly)
	if statusOnly && knownIntent && readErr == nil && len(changes) == 0 {
		return write()
	}
	record := baseRecord(ctx, action, target)
	record.PreviousVersion, record.ExpectedVersion, record.ObservedUID = observed.Version, expectedVersion, observed.UID
	record.Changes, record.Truncated = changes, truncated
	pending, err := c.Store.Begin(ctx, record)
	if err != nil {
		return err
	} // Fail closed before touching Kubernetes.
	if readErr != nil && !apierrors.IsNotFound(readErr) {
		pending.Result, pending.Error = "Failed", "could not read resource before mutation"
		return errors.Join(readErr, c.Store.Finish(ctx, pending))
	}
	err = write()
	pending.Result, pending.Error = outcome(err)
	if err == nil {
		if action != "resource.delete" {
			pending.Resource = c.Resource(obj)
			after, encodeErr := json.Marshal(obj)
			if encodeErr != nil {
				return errors.Join(encodeErr, c.Store.Finish(ctx, pending))
			}
			pending.Changes, pending.Truncated = Diff(before, after, statusOnly)
		}
		if pending.ClusterUID == "" && (pending.Resource.Kind == "PulsarCluster" || pending.Resource.Kind == "BookKeeperCluster") {
			pending.ClusterUID = ClusterUID(obj)
		}
		if pending.Source == "controller" && action != "resource.create" && action != "resource.delete" && len(pending.Changes) == 0 {
			return c.Store.discardNoop(ctx, pending.ID)
		}
	}
	if info := FromContext(ctx); info != nil && info.Source == "api" {
		info.Target = pending.Resource
		info.ClusterUID = pending.ClusterUID
	}
	return errors.Join(err, c.Store.Finish(ctx, pending))
}

func outcome(err error) (string, string) {
	if err == nil {
		return "Succeeded", ""
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		value := status.Status()
		// Do not persist error messages: admission errors may echo secret values.
		reason := "APIError"
		switch value.Reason {
		case "Conflict", "NotFound", "AlreadyExists", "Invalid", "Forbidden", "Unauthorized", "BadRequest", "TooManyRequests", "Timeout", "ServerTimeout", "ServiceUnavailable", "InternalError":
			reason = string(value.Reason)
		}
		if value.Code >= 400 && value.Code < 500 && value.Code != 408 {
			return "Failed", fmt.Sprintf("Kubernetes %d %s", value.Code, reason)
		}
		return "Unknown", fmt.Sprintf("Kubernetes %d %s; verify resource state", value.Code, reason)
	}
	return "Unknown", "mutation outcome is uncertain; verify resource state"
}

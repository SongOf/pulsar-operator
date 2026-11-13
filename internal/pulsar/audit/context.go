package audit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"strconv"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const OperationAnnotation = "pulsar.songof.dev/audit-operation"
const GenerationAnnotation = "pulsar.songof.dev/audit-generation"

type contextKey struct{}
type Info struct {
	OperationID, Source, Actor, ClientIP, ClusterUID string
	Target                                           Resource
}

func FromContext(ctx context.Context) *Info {
	info, _ := ctx.Value(contextKey{}).(*Info)
	return info
}

func RequestContext(ctx context.Context, token, remoteAddress string) context.Context {
	actor := "anonymous"
	if token != "" {
		actor = fmt.Sprintf("console-token:%x", sha256.Sum256([]byte(token)))[:30]
	}
	ip, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		ip = remoteAddress
	}
	return context.WithValue(ctx, contextKey{}, &Info{OperationID: uuid.NewString(), Source: "api", Actor: actor, ClientIP: ip})
}

func ControllerContext(ctx context.Context, controller string, obj client.Object) context.Context {
	op := obj.GetAnnotations()[OperationAnnotation]
	generation, _ := strconv.ParseInt(obj.GetAnnotations()[GenerationAnnotation], 10, 64)
	if op == "" || generation != obj.GetGeneration() {
		op = fmt.Sprintf("system-%s-%d", obj.GetUID(), obj.GetGeneration())
	}
	info := &Info{OperationID: op, Source: "controller", Actor: "controller/" + controller, ClusterUID: ClusterUID(obj)}
	return context.WithValue(ctx, contextKey{}, info)
}

func ClusterUID(obj metav1.Object) string {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.APIVersion == "pulsar.songof.dev/v1alpha1" && owner.Kind == "PulsarCluster" && owner.Controller != nil && *owner.Controller {
			return string(owner.UID)
		}
	}
	return string(obj.GetUID())
}

// Stamp binds a user request to exactly the spec generation it changes. A later
// kubectl edit increments generation and is therefore recorded as a system cause.
func Stamp(ctx context.Context, obj metav1.Object, generation int64) {
	info := FromContext(ctx)
	if info == nil {
		return
	}
	annotations := map[string]string{}
	for key, value := range obj.GetAnnotations() {
		annotations[key] = value
	}
	annotations[OperationAnnotation] = info.OperationID
	annotations[GenerationAnnotation] = strconv.FormatInt(generation, 10)
	obj.SetAnnotations(annotations)
}

func baseRecord(ctx context.Context, action string, target Resource) Record {
	r := Record{Action: action, Resource: target, Source: "controller", Actor: "controller", OperationID: uuid.NewString()}
	if info := FromContext(ctx); info != nil {
		r.OperationID, r.Source, r.Actor, r.ClientIP, r.ClusterUID = info.OperationID, info.Source, info.Actor, info.ClientIP, info.ClusterUID
	}
	return r
}

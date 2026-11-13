package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxClusterNameLength reserves space for the longest Service suffix,
// "-broker-headless". Keep the CRD validation markers aligned with this limit.
const MaxClusterNameLength = 47

const MaxBookKeeperNameLength = 51 // reserve "-bookie-init" for the metadata Job

func ValidateClusterName(name string) error    { return validateName(name, MaxClusterNameLength) }
func ValidateBookKeeperName(name string) error { return validateName(name, MaxBookKeeperNameLength) }

func validateName(name string, limit int) error {
	if len(name) > limit || len(validation.IsDNS1035Label(name)) != 0 {
		return fmt.Errorf("cluster name must be a DNS label starting with a letter and at most %d characters", limit)
	}
	return nil
}

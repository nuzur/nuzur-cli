package deploy

import (
	"errors"
	"fmt"
	"testing"
)

// A delete that fails because the instance is already gone is not a delete that
// failed. Destroying a record whose droplet had been removed underneath it printed
// doctl's "404 ... could not be found" and then "delete it manually to avoid charges"
// — sending the user to hunt through their provider console for a server that does
// not exist, on a bill it is not on. The lookup path has recognised this case all
// along; only the delete path did not.
func TestInstanceAlreadyGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{
			// The exact shape observed, wrapped by the DigitalOcean adapter.
			name: "doctl 404",
			err: fmt.Errorf("deleting droplet 589254659: %w",
				errors.New("Error: DELETE https://api.digitalocean.com/v2/droplets/589254659: 404 (request \"…\") The resource you were accessing could not be found")),
			want: true,
		},
		{name: "hcloud", err: errors.New("deleting Hetzner server 12345: server not found"), want: true},
		{name: "gcloud", err: errors.New("The resource 'projects/x/zones/y/instances/z' was not found"), want: true},
		{name: "az", err: errors.New("(ResourceNotFound) The Resource 'Microsoft.Compute/virtualMachines/nuzur-x' was not found"), want: true},
		{name: "gone", err: errors.New("HTTP 410: instance is gone"), want: true},
		{
			// Everything else is a real failure, and only a real failure may send
			// someone to their provider console.
			name: "a permissions failure is not 'gone'",
			err:  errors.New("deleting droplet 589254659: 403 (request \"…\") You are not authorized to perform this operation"),
			want: false,
		},
		{name: "a timeout is not 'gone'", err: errors.New("context deadline exceeded"), want: false},
		{name: "a missing CLI is not 'gone'", err: errors.New("exec: \"doctl\": executable file not found in $PATH"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstanceAlreadyGone(tc.err); got != tc.want {
				t.Fatalf("InstanceAlreadyGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

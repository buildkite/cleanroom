//go:build !libspore

package sporevm

import "fmt"

func New() (Client, error) {
	return nil, fmt.Errorf("%w (rebuild with -tags libspore and libspore pkg-config installed)", ErrUnavailable)
}

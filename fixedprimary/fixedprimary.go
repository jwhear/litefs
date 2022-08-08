package fixedprimary

import (
	"context"
	"time"
	"math"
	"log"
	"net/http"
	"io"

	"github.com/superfly/litefs"
)


// A simple Leaser which uses a provided URL for the primary
type Leaser struct {
    primaryURL string
    id string
    primaryID string
}

func NewLeaser(primaryURL string, id string) *Leaser {
    return &Leaser{
        primaryURL: primaryURL,
        id: id,
        primaryID: "",
    }
}

func (l *Leaser) Close() (err error) { return nil }

func (l *Leaser) AdvertiseURL() string { return "" }

func (l *Leaser) Acquire(ctx context.Context) (litefs.Lease, error) {
    // Query the primary's instance/id endpoint
    resp, err := http.Get("http://localhost:20101/instance/id");
    if err != nil {
        log.Printf("Failed to reach primary for instance id")
        return nil, litefs.ErrNoPrimary
    }

    defer resp.Body.Close()
    primaryID, err := io.ReadAll(resp.Body)
    l.primaryID = string(primaryID)

    // Is somebody else the primary?
    if !l.IsPrimary() {
        return nil, litefs.ErrPrimaryExists
    }

    // I am the primary, return a lease
    return Lease{
        leaser: l,
        renewedAt: time.Now(),
    }, nil
}

func (l *Leaser) PrimaryURL(ctx context.Context) (string, error) {
    if l.primaryID == "" {
        return "", nil
    } else {
        return l.primaryURL, nil
    }
}

func (l *Leaser) IsPrimary() bool {
    return l.primaryID == l.id
}

type Lease struct {
    leaser    *Leaser
    renewedAt time.Time
}

func (l Lease) RenewedAt() time.Time {
    return l.renewedAt
}

func (l Lease) TTL() time.Duration {
    //TODO might be good to not have this be permanent?
    return math.MaxInt64
}

func (l Lease) Renew(ctx context.Context) error {
    l.renewedAt = time.Now()
    return nil
}

func (l Lease) Close() error { return nil }

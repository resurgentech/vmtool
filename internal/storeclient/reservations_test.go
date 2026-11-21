package storeclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// V5: the client of the reservation records. Placeholders only.

func TestNetworkReservationClient(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	r1 := NetworkReservation{Network: "net1", Mac: "52:54:00:00:00:01", Ip: "10.77.1.100", Host: "hv1", Vm: "web1", Created: now}
	var put NetworkReservationInput
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/networks/net1/reservations/52:54:00:00:00:01":
			if json.Unmarshal(b, &put) != nil {
				js(w, 400, map[string]string{"error": "bad"})
				return
			}
			js(w, 200, r1)
		case "PUT /v1/networks/net1/reservations/52:54:00:00:00:02":
			js(w, 409, map[string]string{"error": "address is taken"})
		case "GET /v1/networks/net1/reservations":
			js(w, 200, map[string]any{"reservations": []NetworkReservation{r1}})
		case "DELETE /v1/networks/net1/reservations/52:54:00:00:00:01":
			w.WriteHeader(204)
		default:
			js(w, 404, map[string]string{"error": "not found"})
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()

	got, err := c.PutNetworkReservation(ctx, "net1", "52:54:00:00:00:01", NetworkReservationInput{Ip: "10.77.1.100", Host: "hv1", Vm: "web1"})
	if err != nil || got.Ip != "10.77.1.100" || put.Vm != "web1" || put.Host != "hv1" {
		t.Fatalf("put = %+v, %v (sent %+v)", got, err, put)
	}
	if _, err := c.PutNetworkReservation(ctx, "net1", "52:54:00:00:00:02", NetworkReservationInput{Ip: "10.77.1.100", Host: "hv1", Vm: "web2"}); !errors.Is(err, ErrRegistryConflict) {
		t.Errorf("taken address: %v, want ErrRegistryConflict", err)
	}
	list, err := c.NetworkReservations(ctx, "net1")
	if err != nil || len(list) != 1 || list[0].Vm != "web1" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := c.NetworkReservations(ctx, "nonet"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown network: %v, want ErrNotFound", err)
	}
	if err := c.DeleteNetworkReservation(ctx, "net1", "52:54:00:00:00:01"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := c.DeleteNetworkReservation(ctx, "net1", "52:54:00:00:00:09"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of none: %v, want ErrNotFound", err)
	}
}

package storeclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jaredeh/vmtool/internal/storeapi"
)

// The network registry (tranche 17, V3): the networks that span hosts, their
// members, and the allocation of VNIs and subnets. User tokens call all of it
// but HostNetworks, the one read a host token may make, for its own host.

type (
	Network            = storeapi.Network
	NetworkInput       = storeapi.NetworkInput
	NetworkMember      = storeapi.NetworkMember
	NetworkMemberInput = storeapi.NetworkMemberInput
	HostNetworks       = storeapi.HostNetworks
	HostNetwork        = storeapi.HostNetwork
	NetworkPeer        = storeapi.NetworkPeer
)

// ErrRegistryConflict is a 409 of a registry route: the name, a given VNI or
// subnet is taken, no VNI or /24 is free, or the network still has members.
// The store's text is not repeated (errors never carry a response body).
var ErrRegistryConflict = errors.New("the registry refused: a name, VNI or subnet is taken or none is free, or the network still has members")

func registryErr(op string, code int) error {
	if code == http.StatusConflict {
		return fmt.Errorf("store %s: %w", op, ErrRegistryConflict)
	}
	return statusErr(op, code)
}

// CreateNetwork registers a network and returns it with its VNI and subnet
// (POST /v1/networks). Vni and Subnet nil are allocated by the store.
func (c *Client) CreateNetwork(ctx context.Context, in NetworkInput) (*Network, error) {
	r, err := c.api.CreateNetworkWithResponse(ctx, in)
	if err != nil {
		return nil, transportErr("create network", err)
	}
	if r.StatusCode() != http.StatusCreated || r.JSON201 == nil {
		return nil, registryErr("create network", r.StatusCode())
	}
	return r.JSON201, nil
}

// Networks lists every registered network with its members (GET /v1/networks).
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	r, err := c.api.ListNetworksWithResponse(ctx)
	if err != nil {
		return nil, transportErr("list networks", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, registryErr("list networks", r.StatusCode())
	}
	return r.JSON200.Networks, nil
}

// GetNetwork is one registered network (GET /v1/networks/{name}); ErrNotFound
// when there is none.
func (c *Client) GetNetwork(ctx context.Context, name string) (*Network, error) {
	r, err := c.api.GetNetworkWithResponse(ctx, name)
	if err != nil {
		return nil, transportErr("get network", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, registryErr("get network", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteNetwork removes a network with no members (DELETE /v1/networks/{name});
// ErrRegistryConflict while it has members, ErrNotFound when there is none.
func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	r, err := c.api.DeleteNetworkWithResponse(ctx, name)
	if err != nil {
		return transportErr("delete network", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return registryErr("delete network", r.StatusCode())
	}
	return nil
}

// PutNetworkMember adds host to a network, or updates its LAN address and
// interface (PUT /v1/networks/{name}/members/{host}); ErrNotFound when the
// network is not registered.
func (c *Client) PutNetworkMember(ctx context.Context, network, host string, in NetworkMemberInput) (*NetworkMember, error) {
	r, err := c.api.PutNetworkMemberWithResponse(ctx, network, host, in)
	if err != nil {
		return nil, transportErr("put network member", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, registryErr("put network member", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteNetworkMember removes host from a network
// (DELETE /v1/networks/{name}/members/{host}); ErrNotFound when it is not one.
func (c *Client) DeleteNetworkMember(ctx context.Context, network, host string) error {
	r, err := c.api.DeleteNetworkMemberWithResponse(ctx, network, host)
	if err != nil {
		return transportErr("delete network member", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return registryErr("delete network member", r.StatusCode())
	}
	return nil
}

// HostNetworks reads the networks host belongs to or is the home of, with its
// peers' LAN addresses (GET /v1/hosts/{host}/networks). A host token may read
// only its own host (ErrForbidden for another's).
func (c *Client) HostNetworks(ctx context.Context, host string) (*HostNetworks, error) {
	r, err := c.api.GetHostNetworksWithResponse(ctx, host)
	if err != nil {
		return nil, transportErr("host networks", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, registryErr("host networks", r.StatusCode())
	}
	return r.JSON200, nil
}

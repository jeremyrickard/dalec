package rpmbundle

import (
	"bytes"
	"context"
	"fmt"

	"github.com/azure/dalec/frontend"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/image"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/frontend/gateway/client"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/frontend/subrequests/targets"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	targetBuildroot = "buildroot"
	targetResolve   = "resolve"
)

type reexecFrontend interface {
	CurrentFrontend() (*llb.State, error)
}

func loadSpec(ctx context.Context, client *dockerui.Client) (*frontend.Spec, error) {
	src, err := client.ReadEntrypoint(ctx, "Dockerfile")
	if err != nil {
		return nil, fmt.Errorf("could not read spec file: %w", err)
	}

	spec, err := frontend.LoadSpec(bytes.TrimSpace(src.Data), client.BuildArgs)
	if err != nil {
		return nil, fmt.Errorf("error loading spec: %w", err)
	}
	return spec, nil
}

func handleSubrequest(ctx context.Context, bc *dockerui.Client) (*client.Result, bool, error) {
	return bc.HandleSubrequest(ctx, dockerui.RequestHandler{
		ListTargets: func(ctx context.Context) (*targets.List, error) {
			_, err := loadSpec(ctx, bc)
			if err != nil {
				return nil, err
			}

			return &targets.List{
				Targets: []targets.Target{
					{
						Name:        targetBuildroot,
						Default:     true,
						Description: "Outputs an rpm buildroot suitable for passing to rpmbuild",
					},
					{
						Name:        targetResolve,
						Description: "Outputs the resolved yaml spec with build args expanded",
					},
				},
			}, nil
		},
	})
}

func Build(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
	bc, err := dockerui.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("could not create build client: %w", err)
	}

	res, handled, err := handleSubrequest(ctx, bc)
	if err != nil || handled {
		return res, err
	}

	rb, err := bc.Build(ctx, func(ctx context.Context, platform *ocispecs.Platform, idx int) (gwclient.Reference, *image.Image, error) {
		spec, err := loadSpec(ctx, bc)
		if err != nil {
			return nil, nil, err
		}

		switch bc.Target {
		case targetBuildroot, "":
			return handleBuildRoot(ctx, client, spec)
		case targetResolve:
			return handleResolve(ctx, client, spec)
		default:
			return nil, nil, fmt.Errorf("unknown target %q", bc.Target)
		}
	})
	if err != nil {
		return nil, err
	}
	return rb.Finalize()
}

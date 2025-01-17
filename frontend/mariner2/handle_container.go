package mariner2

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/dalec"
	"github.com/Azure/dalec/frontend"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	marinerDistrolessRef = "mcr.microsoft.com/cbl-mariner/distroless/base:2.0"
)

func handleContainer(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
	return frontend.BuildWithPlatform(ctx, client, func(ctx context.Context, client gwclient.Client, platform *ocispecs.Platform, spec *dalec.Spec, targetKey string) (gwclient.Reference, *dalec.DockerImageSpec, error) {
		sOpt, err := frontend.SourceOptFromClient(ctx, client)
		if err != nil {
			return nil, nil, err
		}

		pg := dalec.ProgressGroup("Build mariner2 container: " + spec.Name)

		rpmDir, err := specToRpmLLB(spec, sOpt, targetKey, pg)
		if err != nil {
			return nil, nil, fmt.Errorf("error creating rpm: %w", err)
		}

		st, err := specToContainerLLB(spec, targetKey, getWorkerImage(client, pg), rpmDir, sOpt, pg)
		if err != nil {
			return nil, nil, err
		}

		def, err := st.Marshal(ctx, pg)
		if err != nil {
			return nil, nil, fmt.Errorf("error marshalling llb: %w", err)
		}

		res, err := client.Solve(ctx, gwclient.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, nil, err
		}

		base := frontend.GetBaseOutputImage(spec, targetKey, marinerDistrolessRef)
		img, err := frontend.BuildImageConfig(ctx, client, spec, targetKey, base)
		if err != nil {
			return nil, nil, err
		}

		ref, err := res.SingleRef()
		if err != nil {
			return nil, nil, err
		}

		if err := frontend.RunTests(ctx, client, spec, ref, targetKey); err != nil {
			return nil, nil, err
		}

		return ref, img, err
	})
}

func specToContainerLLB(spec *dalec.Spec, target string, builderImg llb.State, rpmDir llb.State, sOpt dalec.SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
	opts = append(opts, dalec.ProgressGroup("Install RPMs"))
	const workPath = "/tmp/rootfs"

	mfstDir := filepath.Join(workPath, "var/lib/rpmmanifest")
	mfst1 := filepath.Join(mfstDir, "container-manifest-1")
	mfst2 := filepath.Join(mfstDir, "container-manifest-2")
	rpmdbDir := filepath.Join(workPath, "var/lib/rpm")

	chrootedPaths := []string{
		filepath.Join(workPath, "/usr/local/bin"),
		filepath.Join(workPath, "/usr/local/sbin"),
		filepath.Join(workPath, "/usr/bin"),
		filepath.Join(workPath, "/usr/sbin"),
		filepath.Join(workPath, "/bin"),
		filepath.Join(workPath, "/sbin"),
	}
	chrootedPathEnv := strings.Join(chrootedPaths, ":")

	installCmd := `
#!/usr/bin/env sh

check_non_empty() {
	ls ${1} > /dev/null 2>&1
}

arch_dir="/tmp/rpms/$(uname -m)"
noarch_dir="/tmp/rpms/noarch"

rpms=""

if check_non_empty "${noarch_dir}/*.rpm"; then
	rpms="${noarch_dir}/*.rpm"
fi

if check_non_empty "${arch_dir}/*.rpm"; then
	rpms="${rpms} ${arch_dir}/*.rpm"
fi

if [ -n "${rpms}" ]; then
	tdnf -v install --releasever=2.0 -y --nogpgcheck --installroot "` + workPath + `" --setopt=reposdir=/etc/yum.repos.d ${rpms} || exit
fi

# If the rpm command is in the rootfs then we don't need to do anything
# If not then this is a distroless image and we need to generate manifests of the installed rpms and cleanup the rpmdb.

PATH="` + chrootedPathEnv + `" command -v rpm && exit 0

set -e
mkdir -p ` + mfstDir + `
rpm --dbpath=` + rpmdbDir + ` -qa > ` + mfst1 + `
rpm --dbpath=` + rpmdbDir + ` -qa --qf "%{NAME}\t%{VERSION}-%{RELEASE}\t%{INSTALLTIME}\t%{BUILDTIME}\t%{VENDOR}\t(none)\t%{SIZE}\t%{ARCH}\t%{EPOCHNUM}\t%{SOURCERPM}\n" > ` + mfst2 + `
rm -rf ` + rpmdbDir + `
`

	installer := llb.Scratch().File(llb.Mkfile("install.sh", 0o755, []byte(installCmd)), opts...)

	baseImg := llb.Image(frontend.GetBaseOutputImage(spec, target, marinerDistrolessRef), llb.WithMetaResolver(sOpt.Resolver), dalec.WithConstraints(opts...))
	worker := builderImg.
		Run(
			shArgs("/tmp/install.sh"),
			defaultTdnfCacheMount(),
			llb.AddMount("/tmp/rpms", rpmDir, llb.SourcePath("/RPMS")),
			llb.AddMount("/tmp/install.sh", installer, llb.SourcePath("install.sh")),
			// Mount the tdnf cache into the workpath so that:
			// 1. tdnf will use the cache
			// 2. Repo data and packages are not left behind in the final image.
			tdnfCacheMountWithPrefix(workPath),
			dalec.WithConstraints(opts...),
		)

	// This adds a mount to the worker so that all the commands are run with this mount added
	// The return value is the state representing the contents of the mounted directory after the commands are run
	rootfs := worker.AddMount(workPath, baseImg)

	if post := getImagePostInstall(spec, target); post != nil && len(post.Symlinks) > 0 {
		rootfs = worker.
			Run(dalec.WithConstraints(opts...), addImagePost(post, workPath)).
			AddMount(workPath, rootfs)
	}

	return rootfs, nil
}

func getImagePostInstall(spec *dalec.Spec, targetKey string) *dalec.PostInstall {
	tgt, ok := spec.Targets[targetKey]
	if ok && tgt.Image != nil && tgt.Image.Post != nil {
		return tgt.Image.Post
	}

	if spec.Image == nil {
		return nil
	}
	return spec.Image.Post
}

func addImagePost(post *dalec.PostInstall, rootfsPath string) llb.RunOption {
	return runOptionFunc(func(ei *llb.ExecInfo) {
		if post == nil {
			return
		}

		if len(post.Symlinks) == 0 {
			return
		}

		buf := bytes.NewBuffer(nil)
		buf.WriteString("set -ex\n")
		fmt.Fprintf(buf, "cd %q\n", rootfsPath)

		for src, tgt := range post.Symlinks {
			fmt.Fprintf(buf, "ln -s %q %q\n", src, filepath.Join(rootfsPath, tgt.Path))
		}
		shArgs(buf.String()).SetRunOption(ei)
		dalec.ProgressGroup("Add post-install symlinks").SetRunOption(ei)
	})
}

type runOptionFunc func(*llb.ExecInfo)

func (f runOptionFunc) SetRunOption(ei *llb.ExecInfo) {
	f(ei)
}

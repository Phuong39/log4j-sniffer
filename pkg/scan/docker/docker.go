// Copyright (c) 2021 Palantir Technologies. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/mholt/archiver/v3"
	"github.com/palantir/log4j-sniffer/pkg/archive"
	"github.com/palantir/log4j-sniffer/pkg/crawl"
	"github.com/palantir/log4j-sniffer/pkg/scan"
	"github.com/pkg/errors"
)

type Scanner struct {
	config     scan.Config
	crawler    crawl.Crawler
	reporter   *crawl.Reporter
	identifier crawl.Identifier
	client     client.CommonAPIClient
}

func NewDockerScanner(config scan.Config, stdout, stderr io.Writer) (*Scanner, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, errors.Wrap(err, "failed to create docker client")
	}

	return &Scanner{
		config: config,
		crawler: crawl.Crawler{
			ErrorWriter: stderr,
			IgnoreDirs:  config.Ignores,
		},
		reporter: &crawl.Reporter{
			OutputJSON:      config.OutputJSON,
			OutputWriter:    stdout,
			DisableCVE45105: config.DisableCVE45105,
		},
		identifier: crawl.NewIdentifier(config.ArchiveListTimeout, archive.WalkZipFiles, archive.WalkTarGzFiles),
		client:     dockerClient,
	}, nil
}

func (d Scanner) ScanImages(ctx context.Context) (int64, error) {
	imageList, err := d.client.ImageList(ctx, dockertypes.ImageListOptions{})
	if err != nil {
		return 0, errors.Wrap(err, "could not list docker images")
	}

	var stats crawl.Stats
	for _, image := range imageList {
		if len(image.RepoTags) == 0 {
			continue
		}
		d.reporter.SetImageID(image.ID)
		d.reporter.SetImageTags(image.RepoTags)
		imageStats, err := d.scanImage(ctx, image)
		if err != nil {
			// write an error and continue scanning other images if we hit an error with this
			// image
			_, _ = fmt.Fprintln(d.crawler.ErrorWriter, err.Error())
			continue
		}
		stats.Append(imageStats)
	}

	count := d.reporter.Count()
	if d.config.OutputSummary {
		if err := scan.WriteSummary(d.reporter.OutputWriter, d.config, stats, count); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (d Scanner) scanImage(ctx context.Context, image dockertypes.ImageSummary) (crawl.Stats, error) {
	ref, err := name.ParseReference(image.RepoTags[0])
	if err != nil {
		return crawl.Stats{}, errors.Wrapf(err, "failed to get image reference")
	}

	img, err := daemon.Image(ref, daemon.WithClient(d.client), daemon.WithContext(ctx))
	if err != nil {
		return crawl.Stats{}, err
	}

	// create a temporary directory where the docker image tarball can be exported to
	imageTmpDir, err := os.MkdirTemp("", fmt.Sprintf("log4j-sniffer-%s", image.ID))
	if err != nil {
		return crawl.Stats{}, errors.Wrap(err, "could not create temporary directory for image")
	}

	outFile, err := os.Create(filepath.Join(imageTmpDir, "image.tar"))
	if err != nil {
		return crawl.Stats{}, err
	}

	defer func() {
		if err := outFile.Close(); err != nil {
			_, _ = fmt.Fprintf(d.crawler.ErrorWriter, "failed to remove temporary image directory %s\n", imageTmpDir)
		}
		if err := os.RemoveAll(imageTmpDir); err != nil {
			_, _ = fmt.Fprintf(d.crawler.ErrorWriter, "failed to remove temporary image directory %s\n", imageTmpDir)
		}
	}()

	// flatten all layers into a single layer and export image to tarball
	if err := crane.Export(img, outFile); err != nil {
		return crawl.Stats{}, errors.Wrap(err, "could not export image")
	}

	// this can be removed when we do recursive tars as we can just scan from the
	// imageTmpDir but for now we have to extract the image tarball
	if err := archiver.Unarchive(outFile.Name(), imageTmpDir); err != nil {
		return crawl.Stats{}, errors.Wrap(err, "failed to extract image")
	}

	// remove the image.tar to avoid duplicate matches
	if err := os.Remove(outFile.Name()); err != nil {
		return crawl.Stats{}, err
	}

	// change to the extracted image directory so paths are reported relative to
	// the root of the container
	if err := os.Chdir(imageTmpDir); err != nil {
		return crawl.Stats{}, err
	}

	return d.crawler.Crawl(ctx, ".", d.identifier.Identify, d.reporter.Collect)
}
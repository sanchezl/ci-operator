package release

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	imageapi "github.com/openshift/api/image/v1"
	"github.com/openshift/ci-operator/pkg/api"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	ConfigMapName = "release"
)

// stableImagesTagStep is used when no release configuration is necessary
type stableImagesTagStep struct {
	jobSpec   *api.JobSpec
	dstClient imageclientset.ImageV1Interface
}

func StableImagesTagStep(dstClient imageclientset.ImageV1Interface, jobSpec *api.JobSpec) api.Step {
	return &stableImagesTagStep{
		dstClient: dstClient,
		jobSpec:   jobSpec,
	}
}

func (s *stableImagesTagStep) Run(ctx context.Context, dry bool) error {
	log.Printf("Will output images to %s:${component}", api.StableImageStream)

	newIS := &imageapi.ImageStream{
		ObjectMeta: meta.ObjectMeta{
			Name: api.StableImageStream,
		},
		Spec: imageapi.ImageStreamSpec{
			LookupPolicy: imageapi.ImageLookupPolicy{
				Local: true,
			},
		},
	}
	if dry {
		istJSON, err := json.MarshalIndent(newIS, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal image stream: %v", err)
		}
		fmt.Printf("%s\n", istJSON)
		return nil
	}
	_, err := s.dstClient.ImageStreams(s.jobSpec.Namespace).Create(newIS)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create stable imagestreamtag: %v", err)
	}
	return nil
}

func (s *stableImagesTagStep) Inputs(ctx context.Context, dry bool) (api.InputDefinition, error) {
	return nil, nil
}

func (s *stableImagesTagStep) Done() (bool, error) { return true, nil }

func (s *stableImagesTagStep) Requires() []api.StepLink { return []api.StepLink{} }

func (s *stableImagesTagStep) Creates() []api.StepLink { return []api.StepLink{api.ReleaseImagesLink()} }

func (s *stableImagesTagStep) Provides() (api.ParameterMap, api.StepLink) { return nil, nil }

func (s *stableImagesTagStep) Name() string { return "[output-images]" }

func (s *stableImagesTagStep) Description() string {
	return fmt.Sprintf("Create the output image stream %s", api.StableImageStream)
}

// releaseImagesTagStep will tag a full release suite
// of images in from the configured namespace. It is
// expected that builds will overwrite these tags at
// a later point, selectively
type releaseImagesTagStep struct {
	config          api.ReleaseTagConfiguration
	srcClient       imageclientset.ImageV1Interface
	dstClient       imageclientset.ImageV1Interface
	routeClient     routeclientset.RoutesGetter
	configMapClient coreclientset.ConfigMapsGetter
	params          *api.DeferredParameters
	jobSpec         *api.JobSpec
}

func findStatusTag(is *imageapi.ImageStream, tag string) (*coreapi.ObjectReference, string) {
	for _, t := range is.Status.Tags {
		if t.Tag != tag {
			continue
		}
		if len(t.Items) == 0 {
			return nil, ""
		}
		if len(t.Items[0].Image) == 0 {
			return &coreapi.ObjectReference{
				Kind: "DockerImage",
				Name: t.Items[0].DockerImageReference,
			}, ""
		}
		return &coreapi.ObjectReference{
			Kind:      "ImageStreamImage",
			Namespace: is.Namespace,
			Name:      fmt.Sprintf("%s@%s", is.Name, t.Items[0].Image),
		}, t.Items[0].Image
	}
	return nil, ""
}

func (s *releaseImagesTagStep) Inputs(ctx context.Context, dry bool) (api.InputDefinition, error) {
	return nil, nil
}

func sourceName(config api.ReleaseTagConfiguration) string {
	if len(config.Name) > 0 {
		return fmt.Sprintf("%s/%s:${component}", config.Namespace, config.Name)
	}
	return fmt.Sprintf("%s/${component}:%s", config.Namespace, config.Tag)
}

func (s *releaseImagesTagStep) Run(ctx context.Context, dry bool) error {
	if dry {
		log.Printf("Tagging release images from %s", sourceName(s.config))
	} else {
		if format, err := s.imageFormat(); err == nil {
			log.Printf("Tagged release images from %s, images will be pullable from %s", sourceName(s.config), format)
		} else {
			log.Printf("Tagged release images from %s", sourceName(s.config))
		}
	}

	if len(s.config.Name) > 0 {
		is, err := s.srcClient.ImageStreams(s.config.Namespace).Get(s.config.Name, meta.GetOptions{})
		if err != nil {
			return fmt.Errorf("could not resolve stable imagestream: %v", err)
		}

		// check to see if the src and dst are the same cluster, in which case we can use a more efficient tagging path
		if len(s.config.Cluster) > 0 {
			if dstIs, err := s.dstClient.ImageStreams(is.Namespace).Get(is.Name, meta.GetOptions{}); err == nil && dstIs.UID == is.UID {
				s.config.Cluster = ""
			}
		}

		var repo string
		if len(s.config.Cluster) > 0 {
			if len(is.Status.PublicDockerImageRepository) > 0 {
				repo = is.Status.PublicDockerImageRepository
			} else if len(is.Status.DockerImageRepository) > 0 {
				repo = is.Status.DockerImageRepository
			} else {
				return fmt.Errorf("remote image stream %s has no accessible image registry value", s.config.Name)
			}
		}

		is.UID = ""
		newIS := &imageapi.ImageStream{
			ObjectMeta: meta.ObjectMeta{
				Name: api.StableImageStream,
			},
			Spec: imageapi.ImageStreamSpec{
				LookupPolicy: imageapi.ImageLookupPolicy{
					Local: true,
				},
			},
		}
		for _, tag := range is.Spec.Tags {
			if valid, image := findStatusTag(is, tag.Name); valid != nil {
				if len(s.config.Cluster) > 0 {
					if len(image) > 0 {
						valid = &coreapi.ObjectReference{Kind: "DockerImage", Name: fmt.Sprintf("%s@%s", repo, image)}
					} else {
						valid = &coreapi.ObjectReference{Kind: "DockerImage", Name: fmt.Sprintf("%s:%s", repo, tag.Name)}
					}
				}
				newIS.Spec.Tags = append(newIS.Spec.Tags, imageapi.TagReference{
					Name: tag.Name,
					From: valid,
				})
			}
		}

		if dry {
			istJSON, err := json.MarshalIndent(newIS, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal image stream: %v", err)
			}
			fmt.Printf("%s\n", istJSON)
			return nil
		}
		is, err = s.dstClient.ImageStreams(s.jobSpec.Namespace).Create(newIS)
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("could not copy stable imagestreamtag: %v", err)
		}

		for _, tag := range is.Spec.Tags {
			spec, ok := resolvePullSpec(is, tag.Name, false)
			if !ok {
				continue
			}
			s.params.Set(componentToParamName(tag.Name), spec)
		}

		return nil
	}

	stableImageStreams, err := s.srcClient.ImageStreams(s.config.Namespace).List(meta.ListOptions{})
	if err != nil {
		return fmt.Errorf("could not resolve stable imagestreams: %v", err)
	}

	for i, stableImageStream := range stableImageStreams.Items {
		log.Printf("Considering stable image stream %s", stableImageStream.Name)
		targetTag := s.config.Tag
		if override, ok := s.config.TagOverrides[stableImageStream.Name]; ok {
			targetTag = override
		}

		// check exactly once to see if the src and dst are the same cluster, in which case we can use a more efficient tagging path
		if i == 0 && len(s.config.Cluster) > 0 {
			if dstIs, err := s.dstClient.ImageStreams(stableImageStream.Namespace).Get(stableImageStream.Name, meta.GetOptions{}); err == nil && dstIs.UID == stableImageStream.UID {
				s.config.Cluster = ""
			}
		}

		var repo string
		if len(s.config.Cluster) > 0 {
			if len(stableImageStream.Status.PublicDockerImageRepository) > 0 {
				repo = stableImageStream.Status.PublicDockerImageRepository
			} else if len(stableImageStream.Status.DockerImageRepository) > 0 {
				repo = stableImageStream.Status.DockerImageRepository
			} else {
				return fmt.Errorf("remote image stream %s has no accessible image registry value", s.config.Name)
			}
		}

		for _, tag := range stableImageStream.Spec.Tags {
			if tag.Name == targetTag {
				log.Printf("Cross-tagging %s:%s from %s/%s:%s", stableImageStream.Name, targetTag, stableImageStream.Namespace, stableImageStream.Name, targetTag)
				var id string
				for _, tagStatus := range stableImageStream.Status.Tags {
					if tagStatus.Tag == targetTag {
						id = tagStatus.Items[0].Image
					}
				}
				if len(id) == 0 {
					return fmt.Errorf("no image found backing %s/%s:%s", stableImageStream.Namespace, stableImageStream.Name, targetTag)
				}
				ist := &imageapi.ImageStreamTag{
					ObjectMeta: meta.ObjectMeta{
						Namespace: s.jobSpec.Namespace,
						Name:      fmt.Sprintf("%s:%s", stableImageStream.Name, targetTag),
					},
					Tag: &imageapi.TagReference{
						Name: targetTag,
						From: &coreapi.ObjectReference{
							Kind:      "ImageStreamImage",
							Name:      fmt.Sprintf("%s@%s", stableImageStream.Name, id),
							Namespace: s.config.Namespace,
						},
					},
				}

				if len(s.config.Cluster) > 0 {
					ist.Tag.From = &coreapi.ObjectReference{Kind: "DockerImage", Name: fmt.Sprintf("%s@%s", repo, id)}
				}

				if dry {
					istJSON, err := json.MarshalIndent(ist, "", "  ")
					if err != nil {
						return fmt.Errorf("failed to marshal imagestreamtag: %v", err)
					}
					fmt.Printf("%s\n", istJSON)
					continue
				}
				ist, err := s.dstClient.ImageStreamTags(s.jobSpec.Namespace).Create(ist)
				if err != nil && !errors.IsAlreadyExists(err) {
					return fmt.Errorf("could not copy stable imagestreamtag: %v", err)
				}

				if spec, ok := resolvePullSpec(&stableImageStream, tag.Name, false); ok {
					s.params.Set(componentToParamName(tag.Name), spec)
				}
			}
		}
	}

	return nil
}

func (s *releaseImagesTagStep) Done() (bool, error) {
	return false, nil
}

func (s *releaseImagesTagStep) Requires() []api.StepLink {
	return []api.StepLink{}
}

func (s *releaseImagesTagStep) Creates() []api.StepLink {
	return []api.StepLink{api.ReleaseImagesLink()}
}

func (s *releaseImagesTagStep) Provides() (api.ParameterMap, api.StepLink) {
	return api.ParameterMap{
		"IMAGE_FORMAT": s.imageFormat,
	}, api.ImagesReadyLink()
}

func (s *releaseImagesTagStep) imageFormat() (string, error) {
	spec, err := s.repositoryPullSpec()
	if err != nil {
		return "REGISTRY", err
	}
	registry := strings.SplitN(spec, "/", 2)[0]
	var format string
	if len(s.config.Name) > 0 {
		format = fmt.Sprintf("%s/%s/%s:%s", registry, s.jobSpec.Namespace, fmt.Sprintf("%s%s", s.config.NamePrefix, api.StableImageStream), api.ComponentFormatReplacement)
	} else {
		format = fmt.Sprintf("%s/%s/%s:%s", registry, s.jobSpec.Namespace, fmt.Sprintf("%s%s", s.config.NamePrefix, api.ComponentFormatReplacement), s.config.Tag)
	}
	return format, nil
}

func (s *releaseImagesTagStep) repositoryPullSpec() (string, error) {
	is, err := s.dstClient.ImageStreams(s.jobSpec.Namespace).Get(api.PipelineImageStream, meta.GetOptions{})
	if err != nil {
		return "", err
	}
	if len(is.Status.PublicDockerImageRepository) > 0 {
		return is.Status.PublicDockerImageRepository, nil
	}
	if len(is.Status.DockerImageRepository) > 0 {
		return is.Status.DockerImageRepository, nil
	}
	return "", fmt.Errorf("no pull spec available for image stream %s", api.PipelineImageStream)
}

func (s *releaseImagesTagStep) Name() string { return "[release-inputs]" }

func (s *releaseImagesTagStep) Description() string {
	return fmt.Sprintf("Find all of the input images from %s and tag them into the output image stream", sourceName(s.config))
}

func ReleaseImagesTagStep(config api.ReleaseTagConfiguration, srcClient, dstClient imageclientset.ImageV1Interface, routeClient routeclientset.RoutesGetter, configMapClient coreclientset.ConfigMapsGetter, params *api.DeferredParameters, jobSpec *api.JobSpec) api.Step {
	// when source and destination client are the same, we don't need to use external imports
	if srcClient == dstClient {
		config.Cluster = ""
	}
	return &releaseImagesTagStep{
		config:          config,
		srcClient:       srcClient,
		dstClient:       dstClient,
		routeClient:     routeClient,
		configMapClient: configMapClient,
		params:          params,
		jobSpec:         jobSpec,
	}
}

func componentToParamName(component string) string {
	return strings.ToUpper(strings.Replace(component, "-", "_", -1))
}

func resolvePullSpec(is *imageapi.ImageStream, tag string, requireExact bool) (string, bool) {
	for _, tags := range is.Status.Tags {
		if tags.Tag != tag {
			continue
		}
		if len(tags.Items) == 0 {
			break
		}
		if image := tags.Items[0].Image; len(image) > 0 {
			if len(is.Status.PublicDockerImageRepository) > 0 {
				return fmt.Sprintf("%s@%s", is.Status.PublicDockerImageRepository, image), true
			}
			if len(is.Status.DockerImageRepository) > 0 {
				return fmt.Sprintf("%s@%s", is.Status.DockerImageRepository, image), true
			}
		}
		break
	}
	if requireExact {
		return "", false
	}
	if len(is.Status.PublicDockerImageRepository) > 0 {
		return fmt.Sprintf("%s:%s", is.Status.PublicDockerImageRepository, tag), true
	}
	if len(is.Status.DockerImageRepository) > 0 {
		return fmt.Sprintf("%s:%s", is.Status.DockerImageRepository, tag), true
	}
	return "", false
}

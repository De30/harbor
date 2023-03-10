// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package base

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	common_http "github.com/goharbor/harbor/src/common/http"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/pkg/reg/filter"
	"github.com/goharbor/harbor/src/pkg/reg/model"
)

type label struct {
	Name string `json:"name"`
}

type chartVersion struct {
	Version string   `json:"version"`
	Labels  []*label `json:"labels"`
}

type chartVersionDetail struct {
	Metadata *chartVersionMetadata `json:"metadata"`
}

type chartVersionMetadata struct {
	URLs []string `json:"urls"`
}

// FetchCharts fetches charts
func (a *Adapter) FetchCharts(filters []*model.Filter) ([]*model.Resource, error) {
	projects, err := a.ListProjects(filters)
	if err != nil {
		return nil, err
	}

	resources := []*model.Resource{}
	for _, project := range projects {
		url := fmt.Sprintf("%s/api/chartrepo/%s/charts", a.Client.GetURL(), project.Name)
		repositories := []*model.Repository{}
		if err := a.httpClient.Get(url, &repositories); err != nil {
			return nil, err
		}
		if len(repositories) == 0 {
			continue
		}
		for _, repository := range repositories {
			repository.Name = fmt.Sprintf("%s/%s", project.Name, repository.Name)
		}
		repositories, err = filter.DoFilterRepositories(repositories, filters)
		if err != nil {
			return nil, err
		}

		for _, repository := range repositories {
			name := strings.SplitN(repository.Name, "/", 2)[1]
			url := fmt.Sprintf("%s/api/chartrepo/%s/charts/%s", a.Client.GetURL(), project.Name, name)
			versions := []*chartVersion{}
			if err := a.httpClient.Get(url, &versions); err != nil {
				return nil, err
			}
			if len(versions) == 0 {
				continue
			}
			var artifacts []*model.Artifact
			for _, version := range versions {
				var labels []string
				for _, label := range version.Labels {
					labels = append(labels, label.Name)
				}
				artifacts = append(artifacts, &model.Artifact{
					Tags:   []string{version.Version},
					Labels: labels,
				})
			}
			artifacts, err = filter.DoFilterArtifacts(artifacts, filters)
			if err != nil {
				return nil, err
			}
			if len(artifacts) == 0 {
				continue
			}

			for _, artifact := range artifacts {
				resources = append(resources, &model.Resource{
					Type:     model.ResourceTypeChart,
					Registry: a.Registry,
					Metadata: &model.ResourceMetadata{
						Repository: &model.Repository{
							Name:     repository.Name,
							Metadata: project.Metadata,
						},
						Artifacts: []*model.Artifact{artifact},
					},
				})
			}
		}
	}
	return resources, nil
}

// ChartExist checks the existence of the chart
func (a *Adapter) ChartExist(name, version string) (bool, error) {
	_, err := a.getChartInfo(name, version)
	if err == nil {
		return true, nil
	}
	if httpErr, ok := err.(*common_http.Error); ok && httpErr.Code == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (a *Adapter) getChartInfo(name, version string) (*chartVersionDetail, error) {
	project, name, err := parseChartName(name)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/api/chartrepo/%s/charts/%s/%s", a.Client.GetURL(), project, name, version)
	info := &chartVersionDetail{}
	if err = a.httpClient.Get(url, info); err != nil {
		return nil, err
	}
	return info, nil
}

// DownloadChart downloads the specific chart
func (a *Adapter) DownloadChart(name, version, contentURL string) (io.ReadCloser, error) {
	info, err := a.getChartInfo(name, version)
	if err != nil {
		return nil, err
	}
	if info.Metadata == nil || len(info.Metadata.URLs) == 0 || len(info.Metadata.URLs[0]) == 0 {
		return nil, fmt.Errorf("cannot got the download url for chart %s:%s", name, version)
	}

	url, err := url.Parse(info.Metadata.URLs[0])
	if err != nil {
		return nil, err
	}
	// relative URL
	urlStr := url.String()
	if !(url.Scheme == "http" || url.Scheme == "https") {
		project, _, err := parseChartName(name)
		if err != nil {
			return nil, err
		}
		urlStr = fmt.Sprintf("%s/chartrepo/%s/%s", a.Client.GetURL(), project, urlStr)
	}
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return nil, errors.Errorf("failed to download the chart %q: %d %s", req.URL.String(), resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// UploadChart uploads the chart
func (a *Adapter) UploadChart(name, version string, chart io.Reader) error {
	project, name, err := parseChartName(name)
	if err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	fw, err := w.CreateFormFile("chart", name+".tgz")
	if err != nil {
		return err
	}
	if _, err = io.Copy(fw, chart); err != nil {
		return err
	}
	w.Close()

	url := fmt.Sprintf("%s/api/chartrepo/%s/charts", a.Client.GetURL(), project)

	req, err := http.NewRequest(http.MethodPost, url, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &common_http.Error{
			Code:    resp.StatusCode,
			Message: string(data),
		}
	}
	return nil
}

// DeleteChart deletes the chart
func (a *Adapter) DeleteChart(name, version string) error {
	project, name, err := parseChartName(name)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/chartrepo/%s/charts/%s/%s", a.Client.GetURL(), project, name, version)
	return a.httpClient.Delete(url)
}

// TODO merge this method and utils.ParseRepository?
func parseChartName(name string) (string, string, error) {
	strs := strings.Split(name, "/")
	if len(strs) == 2 && len(strs[0]) > 0 && len(strs[1]) > 0 {
		return strs[0], strs[1], nil
	}
	return "", "", fmt.Errorf("invalid chart name format: %s", name)
}

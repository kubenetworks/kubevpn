package main

import (
	"net/http"
)

//Source is a release source interface
type Source interface {
	Parse(r *http.Request) (*ReleaseRequest, error)
}

//ReleaseRequest is the release request for new plugin
type ReleaseRequest struct {
	TagName            string `json:"tagName"`
	PluginName         string `json:"pluginName"`
	PluginRepo         string `json:"pluginRepo"`
	PluginOwner        string `json:"pluginOwner"`
	PluginReleaseActor string `json:"pluginReleaseActor"`
	TemplateFile       string `json:"templateFile"`
	ProcessedTemplate  []byte `json:"processedTemplate"`
}

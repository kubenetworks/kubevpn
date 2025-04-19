// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// CIJobName indicates the name of the CI job.
	CIJobName = "ci.job.name"

	// CIJobURL indicates the URL of the CI job.
	CIJobURL = "ci.job.url"

	// CIPipelineID indicates the ID of the CI pipeline.
	CIPipelineID = "ci.pipeline.id"

	// CIPipelineName indicates the name of the CI pipeline.
	CIPipelineName = "ci.pipeline.name"

	// CIPipelineNumber indicates the number of the CI pipeline.
	CIPipelineNumber = "ci.pipeline.number"

	// CIPipelineURL indicates the URL of the CI pipeline.
	CIPipelineURL = "ci.pipeline.url"

	// CIProviderName indicates the name of the CI provider.
	CIProviderName = "ci.provider.name"

	// CIStageName indicates the name of the CI stage.
	CIStageName = "ci.stage.name"

	// CINodeName indicates the name of the node in the CI environment.
	CINodeName = "ci.node.name"

	// CINodeLabels indicates the labels associated with the node in the CI environment.
	CINodeLabels = "ci.node.labels"

	// CIWorkspacePath records an absolute path to the directory where the project has been checked out.
	CIWorkspacePath = "ci.workspace_path"

	// CIEnvVars contains environment variables used to get the pipeline correlation ID.
	CIEnvVars = "_dd.ci.env_vars"
)

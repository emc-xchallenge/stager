package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry-incubator/runtime-schema/router"
	"github.com/cloudfoundry/gunk/urljoiner"
)

const (
	DockerTaskDomain                     = "cf-app-docker-staging"
	DockerCircusFilename                 = "docker-circus.zip"
	DockerStagingRequestsNatsSubject     = "diego.docker.staging.start"
	DockerStagingRequestsReceivedCounter = metric.Counter("DockerStagingRequestsReceived")
	DockerTailorExecutablePath           = "/tmp/docker-circus/tailor"
	DockerTailorOutputPath               = "/tmp/docker-result/result.json"
)

var ErrMissingDockerImageUrl = errors.New("missing docker image download url")

type dockerBackend struct {
	config Config
}

func NewDockerBackend(config Config) Backend {
	return &dockerBackend{
		config: config,
	}
}

func (backend *dockerBackend) StagingRequestsNatsSubject() string {
	return DockerStagingRequestsNatsSubject
}

func (backend *dockerBackend) StagingRequestsReceivedCounter() metric.Counter {
	return DockerStagingRequestsReceivedCounter
}

func (backend *dockerBackend) TaskDomain() string {
	return DockerTaskDomain
}

func (backend *dockerBackend) BuildRecipe(requestJson []byte) (receptor.TaskCreateRequest, error) {
	var request cc_messages.DockerStagingRequestFromCC
	err := json.Unmarshal(requestJson, &request)
	if err != nil {
		return receptor.TaskCreateRequest{}, err
	}

	err = backend.validateRequest(request)
	if err != nil {
		return receptor.TaskCreateRequest{}, err
	}

	compilerURL, err := backend.compilerDownloadURL(request)
	if err != nil {
		return receptor.TaskCreateRequest{}, err
	}

	actions := []models.ExecutorAction{}

	//Download tailor
	actions = append(
		actions,
		models.EmitProgressFor(
			models.ExecutorAction{
				models.DownloadAction{
					From:     compilerURL.String(),
					To:       path.Dir(DockerTailorExecutablePath),
					CacheKey: "tailor-docker",
				},
			},
			"",
			"",
			"Failed to Download Tailor",
		),
	)

	var fileDescriptorLimit *uint64
	if request.FileDescriptors != 0 {
		fd := max(uint64(request.FileDescriptors), backend.config.MinFileDescriptors)
		fileDescriptorLimit = &fd
	}

	//Run Smelter
	actions = append(
		actions,
		models.EmitProgressFor(
			models.ExecutorAction{
				models.RunAction{
					Path:    DockerTailorExecutablePath,
					Args:    []string{"-outputMetadataJSONFilename", DockerTailorOutputPath, "-dockerRef", request.DockerImageUrl},
					Env:     request.Environment.BBSEnvironment(),
					Timeout: 15 * time.Minute,
					ResourceLimits: models.ResourceLimits{
						Nofile: fileDescriptorLimit,
					},
				},
			},
			"Staging...",
			"Staging Complete",
			"Staging Failed",
		),
	)

	annotationJson, _ := json.Marshal(models.StagingTaskAnnotation{
		AppId:  request.AppId,
		TaskId: request.TaskId,
	})

	task := receptor.TaskCreateRequest{
		ResultFile:            DockerTailorOutputPath,
		TaskGuid:              backend.taskGuid(request),
		Domain:                DockerTaskDomain,
		Stack:                 request.Stack,
		MemoryMB:              int(max(uint64(request.MemoryMB), uint64(backend.config.MinMemoryMB))),
		DiskMB:                int(max(uint64(request.DiskMB), uint64(backend.config.MinDiskMB))),
		Actions:               actions,
		CompletionCallbackURL: backend.config.CallbackURL,
		Log: receptor.LogConfig{
			Guid:       request.AppId,
			SourceName: "STG",
		},
		Annotation: string(annotationJson),
	}

	return task, nil
}

func (backend *dockerBackend) BuildStagingResponseFromRequestError(requestJson []byte, errorMessage string) ([]byte, error) {
	request := cc_messages.DockerStagingRequestFromCC{}

	err := json.Unmarshal(requestJson, &request)
	if err != nil {
		return nil, err
	}

	response := cc_messages.DockerStagingResponseForCC{
		AppId:  request.AppId,
		TaskId: request.TaskId,
		Error:  errorMessage,
	}

	return json.Marshal(response)
}

func (backend *dockerBackend) BuildStagingResponse(taskResponse receptor.TaskResponse) ([]byte, error) {
	var response cc_messages.DockerStagingResponseForCC

	var annotation models.StagingTaskAnnotation
	err := json.Unmarshal([]byte(taskResponse.Annotation), &annotation)
	if err != nil {
		return nil, err
	}

	response.AppId = annotation.AppId
	response.TaskId = annotation.TaskId

	if taskResponse.Failed {
		response.Error = taskResponse.FailureReason
	} else {
		var result models.StagingDockerResult
		err := json.Unmarshal([]byte(taskResponse.Result), &result)
		if err != nil {
			return nil, err
		}

		response.ExecutionMetadata = result.ExecutionMetadata
		response.DetectedStartCommand = result.DetectedStartCommand
	}

	return json.Marshal(response)
}

func (backend *dockerBackend) compilerDownloadURL(request cc_messages.DockerStagingRequestFromCC) (*url.URL, error) {

	var circusFilename string
	if len(backend.config.DockerCircusPath) > 0 {
		circusFilename = backend.config.DockerCircusPath
	} else {
		circusFilename = DockerCircusFilename
	}
	parsed, err := url.Parse(circusFilename)
	if err != nil {
		return nil, errors.New("couldn't parse compiler URL")
	}

	switch parsed.Scheme {
	case "http", "https":
		return parsed, nil
	case "":
		break
	default:
		return nil, errors.New("wTF")
	}

	staticRoute, ok := router.NewFileServerRoutes().RouteForHandler(router.FS_STATIC)
	if !ok {
		return nil, errors.New("couldn't generate the compiler download path")
	}

	urlString := urljoiner.Join(backend.config.FileServerURL, staticRoute.Path, circusFilename)

	url, err := url.ParseRequestURI(urlString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse compiler download URL: %s", err)
	}

	return url, nil
}

func (backend *dockerBackend) taskGuid(request cc_messages.DockerStagingRequestFromCC) string {
	return fmt.Sprintf("%s-%s", request.AppId, request.TaskId)
}

func (backend *dockerBackend) validateRequest(stagingRequest cc_messages.DockerStagingRequestFromCC) error {
	if len(stagingRequest.AppId) == 0 {
		return ErrMissingAppId
	}

	if len(stagingRequest.TaskId) == 0 {
		return ErrMissingTaskId
	}

	if len(stagingRequest.DockerImageUrl) == 0 {
		return ErrMissingDockerImageUrl
	}

	return nil
}
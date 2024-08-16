package services

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"go.mongodb.org/mongo-driver/bson/primitive"

	// Internal imports
	"github.com/NeRF-or-Nothing/VidGoNerf/webserver/internal/common"
	"github.com/NeRF-or-Nothing/VidGoNerf/webserver/internal/models/scene"
	"github.com/NeRF-or-Nothing/VidGoNerf/webserver/internal/models/user"
)

type ClientService struct {
	mqService    *AMPQService
	sceneManager *scene.SceneManager
	userManager  *user.UserManager
}

func NewClientService(sceneManager *scene.SceneManager, mqService *AMPQService, userManager *user.UserManager) *ClientService {
	return &ClientService{
		mqService:    mqService,
		sceneManager: sceneManager,
		userManager:  userManager,
	}
}

// verifyUserAccess checks if the given user has access to the given scene.
// Returns nil if the user has access, error if the user does not have access or an error occurred.
func (s *ClientService) verifyUserAccess(ctx context.Context, userID, sceneID primitive.ObjectID) error {
	authorized, err := s.userManager.UserHasJobAccess(ctx, userID, sceneID)
	if err != nil {
		return err
	}
	if !authorized {
		return user.ErrUserNoAccess
	}
	return nil
}

// LoginUser checks if the given username and password are correct and returns the user's ID, nil if successful.
// Returns "", error if the username or password is incorrect.
func (s *ClientService) LoginUser(ctx context.Context, username, password string) (string, error) {
	user, err := s.userManager.GetUserByUsername(ctx, username)
	if err != nil {
		return "", err
	}

	err = user.CheckPassword(password)
	if err != nil {
		return "", err
	}

	return user.ID.Hex(), nil
}

// RegisterUser generates a new user document with the given username and password, and inserts it into the database.
// Returns nil if successful, error if the username is already taken or an error occurred while inserting the user.
func (s *ClientService) RegisterUser(ctx context.Context, username, password string) error {
	_, err := s.userManager.GenerateUser(ctx, username, password)
	if err != nil {
		return err
	}

	return nil
}

// Metadata about a single resource available for a scene.
type ResourceInfo struct {
	Exists        bool  `json:"exists"`
	Size          int64 `json:"size,omitempty"`
	Chunks        int   `json:"chunks,omitempty"`
	LastChunkSize int64 `json:"last_chunk_size,omitempty"`
}

// Metadata about the resources available for a scene.
type NerfMetadata struct {
	Resources map[string]map[string]ResourceInfo `json:"resources"`
}

// GetNerfMetadata returns metadata about the resources available for the given scene.
// Returns error if the user does not have access to the scene or an error occurred.
// For each available output file type, it returns a map of iteration numbers to file information.
// Specifically, it returns whether the file exists, its size, number of (1 MB) chunks, and size of the last chunk.
func (s *ClientService) GetNerfMetadata(ctx context.Context, userID, sceneID primitive.ObjectID, outputType string) (interface{}, error) {
	if err := s.verifyUserAccess(ctx, userID, sceneID); err != nil {
		return nil, err
	}

	nerf, err := s.sceneManager.GetNerf(ctx, sceneID)
	if err != nil {
		return nil, err
	}

	config, err := s.sceneManager.GetTrainingConfig(ctx, sceneID)
	if err != nil {
		return nil, err
	}

	metadata := &NerfMetadata{
		Resources: make(map[string]map[string]ResourceInfo),
	}

	for _, ot := range config.NerfTrainingConfig.OutputTypes {
		if outputType == "" || outputType == ot {

			metadata.Resources[ot] = make(map[string]ResourceInfo)
			filePaths := nerf.GetFilePathsForOutputType(ot)

			for iteration, path := range filePaths {
				info := ResourceInfo{Exists: false}

				if fileInfo, err := os.Stat(path); err == nil {

					fileSize := fileInfo.Size()
					chunks := (fileSize + 1024*1024 - 1) / (1024 * 1024)
					lastChunkSize := fileSize % (1024 * 1024)
					if lastChunkSize == 0 {
						lastChunkSize = 1024 * 1024
					}

					info = ResourceInfo{
						Exists:        true,
						Size:          fileSize,
						Chunks:        int(chunks),
						LastChunkSize: lastChunkSize,
					}
				}

				metadata.Resources[ot][iteration] = info
			}
		}
	}

	return metadata, nil
}

func (s *ClientService) HandleIncomingVideo(ctx context.Context, userID primitive.ObjectID, req common.VideoUploadRequest) (string, error) {
	// Validate video file
    fileStat, err := (*req.File).Stat()
    if err != nil {
        return "", fmt.Errorf("failed to get file stats: %w", err)
    }

    fileName := fileStat.Name()
    if fileName == "" {
        return "", fmt.Errorf("file not received")
    }
    
	fileExt := filepath.Ext(fileName)
	if fileExt != ".mp4" {
		return "", fmt.Errorf("improper file extension")
	}

    jobID := primitive.NewObjectID().Hex()

	// Save video to file storage
	videoName := jobID + ".mp4"
	videosFolder := "data/raw/videos"
	if err := os.MkdirAll(videosFolder, os.ModePerm); err != nil {
		return "", err
	}
	videoFilePath := filepath.Join(videosFolder, videoName)

	dst, err := os.Create(videoFilePath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	src, err := req.File.(*multipart.FileHeader).Open()
	if err != nil {
		return "", err
	}
	defer src.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return "", err
	}

	// Create video and training config
	video := &scene.Video{FilePath: videoFilePath}
	trainingConfig := &scene.TrainingConfig{
		NerfTrainingConfig: &scene.NerfTrainingConfig{
			TrainingMode:    req.TrainingMode,
			OutputTypes:     req.OutputTypes,
			SaveIterations:  req.SaveIterations,
			TotalIterations: req.TotalIterations,
		},
	}

	// Save video to database and create config
	if err := s.sceneManager.SetVideo(ctx, jobID, video); err != nil {
		return "", err
	}

	if err := s.sceneManager.SetSceneName(ctx, jobID, req.SceneName); err != nil {
		return "", err
	}

	if err := s.sceneManager.SetTrainingConfig(ctx, jobID, trainingConfig); err != nil {
		return "", err
	}

	if err := s.mqService.PublishSfmJob(ctx, jobID, video, trainingConfig); err != nil {
		s.loger.Errorf("Failed to publish SFM job: %v", err)
		return "", err
	}

	user, err := s.userManager.GetUserByID(ctx, userID)
	if err != nil {
		return "", err
	}

	user.AddScene(jobID)
	if err := s.userManager.UpdateUser(ctx, user); err != nil {
		return "", err
	}

	return jobID, nil
}

func (s *ClientService) GetNerfResource(ctx context.Context, userID, sceneID primitive.ObjectID, resourceType, iteration, rangeHeader string) {
	return nil
}

func (s *ClientService) GetUserHistory(ctx context.Context, userID primitive.ObjectID) {
	return nil
}

func (s *ClientService) GetScenePreview(ctx context.Context, userID, sceneID primitive.ObjectID) {
	return nil
}

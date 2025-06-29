package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	http.MaxBytesReader(w, r.Body, maxMemory)
	r.ParseMultipartForm(maxMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not the owner of the video", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	data, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to save tmp file", nil)
		return
	}
	defer os.Remove(data.Name()) // Clean up
	defer data.Close()

	_, err = io.Copy(data, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", nil)
		return
	}

	_, err = data.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't seek data", nil)
		return
	}
	//checking video, and formatting
	directory := ""
	aspectRatio, err := getVideoAspectRatio(data.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error determining aspect ratio", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}
	fast_data, err := processVideoForFastStart(data.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing Video for fast start", err)
		return
	}
	fast_open, err := os.Open(fast_data)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open the new file", err)
		return
	}
	defer os.Remove(fast_open.Name())
	defer fast_open.Close()

	key := getAssetPath(mediaType)
	key = filepath.Join(directory, key)
	// uploading to s3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        fast_open,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't put into bucket", nil)
		return
	}
	//url stuff
	url := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, key)
	video.VideoURL = &url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}

	if len(output.Streams) == 0 {
		return "", errors.New("no video streams found")
	}

	width := output.Streams[0].Width
	height := output.Streams[0].Height

	if width == 16*height/9 {
		return "16:9", nil
	} else if height == 16*width/9 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	filePath_processing := filePath + ".processing.mp4"
	cmd := exec.Command("ffmpeg",
		"-i", filePath, "-c",
		"copy", "-movflags",
		"faststart", "-f", "mp4",
		filePath_processing,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg error: %v", err)
	}

	return filePath_processing, nil
}

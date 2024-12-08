package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	_ "image/jpeg"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"time"
)

type RequestModel struct {
	FileUrls []string `json:"fileUrls"`
}

type ResponseModel struct {
	Url string `json:"recapUrl"`
}

func createHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // 주의: 보안에 영향을 미칠 수 있습니다.
			},
		},
	}
}

var httpWebClient = createHttpClient()

func DownloadImage(url string, index int, basepath string, downloadedFilePaths *[]string, mutex *sync.Mutex, client *http.Client) error {
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch the URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download image: status code %d", resp.StatusCode)
	}

	filepath := basepath + "/" + fmt.Sprintf("%02d", index) + ".jpeg"
	outFile, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer outFile.Close()

	_, saveErr := io.Copy(outFile, resp.Body)
	if saveErr != nil {
		return fmt.Errorf("failed to save image: %w", saveErr)
	}

	mutex.Lock()
	*downloadedFilePaths = append(*downloadedFilePaths, filepath)
	mutex.Unlock()
	fmt.Println("이미지 다운로드 완료:", filepath)

	return nil
}

func calculateFrameRate(photoCount int) (float64, error) {
	if photoCount >= 2 && photoCount <= 10 {
		return float64(photoCount) / 5.0, nil
	} else {
		return 0, errors.New("리캡의 사진 수가 허용 범위를 벗어났습니다.")
	}
}

func downloadFromUrl(imgUrl string, index int, basepath string, downloadedFilePaths *[]string, wg *sync.WaitGroup, mutex *sync.Mutex, client *http.Client, s3Client *s3.Client) {
	defer wg.Done()

	parsedURL, parseErr := url.Parse(imgUrl)
	if parseErr != nil {
		fmt.Println("Error parsing URL:", parseErr)
		return
	}
	log.Println("Updating ACL from ", parsedURL.Path)

	input := &s3.PutObjectAclInput{
		Bucket: aws.String("mafoo"),
		Key:    aws.String(parsedURL.Path[1:]),
		ACL:    "public-read",
	}
	_, upErr := s3Client.PutObjectAcl(context.Background(), input)
	if upErr != nil {
		log.Printf("Failed to ACL update image to S3: %v\n", upErr)
		return
	}

	err := DownloadImage(imgUrl, index, basepath, downloadedFilePaths, mutex, client)
	if err != nil {
		log.Printf("Failed to download image from %s: %v\n", imgUrl, err)
	}
}

func uploadFileToS3(bucketName, filePath, s3Key string, s3Client *s3.Client) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("unable to open file %v, %v", filePath, err)
	}
	defer file.Close()

	_, err = s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: &bucketName,
		Key:    &s3Key,
		Body:   file,
		ACL:    "public-read",
	})
	if err != nil {
		return fmt.Errorf("unable to upload file to S3, %v", err)
	}
	return nil
}

func HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	currentTimeMillis := time.Now().UnixNano() / int64(time.Millisecond)
	id := uuid.New()
	basepath := fmt.Sprintf("/tmp/%s", id)
	err := os.Mkdir(basepath, 0755)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, err
	}
	cfg, s3Err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("kr-standard"),
		config.WithCredentialsProvider(aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(os.Getenv("ACCESS_KEY"), os.Getenv("SECRET_KEY"), ""),
		)),
		config.WithHTTPClient(httpWebClient),
		config.WithEndpointResolver(aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
			return aws.Endpoint{URL: "https://kr.object.ncloudstorage.com"}, nil
		})),
	)
	if s3Err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, fmt.Errorf("unable to load SDK config, %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)

	var requestModel RequestModel
	decodeErr := json.Unmarshal([]byte(request.Body), &requestModel)
	if decodeErr != nil {
		log.Printf("Error unmarshalling JSON: %v", decodeErr)
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Body:       `{"error": "Invalid JSON format"}`,
		}, nil
	}

	var mutex sync.Mutex
	var wg sync.WaitGroup
	var downloadedFilePaths []string
	for idx, imgUrl := range requestModel.FileUrls {
		wg.Add(1)
		go downloadFromUrl(imgUrl, idx, basepath, &downloadedFilePaths, &wg, &mutex, httpWebClient, s3Client)
	}
	wg.Wait()
	currentTimeMillis = time.Now().UnixNano() / int64(time.Millisecond)

	framerate, err := calculateFrameRate(len(downloadedFilePaths))
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, err
	}

	cmd := exec.Command("/opt/ffmpeg/ffmpeg", "-framerate", fmt.Sprintf("%f", framerate), "-i", basepath+"/%2d.jpeg", "-c:v", "libx264", "-vf", "crop=trunc\\(iw/2\\)\\*2:trunc\\(ih/2\\)\\*2", "-t", "5", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-x264opts", "keyint=30", basepath+"/output.mp4")
	fmt.Println("FFmpeg 실행:", cmd.String())

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	executionErr := cmd.Run()
	if executionErr != nil {
		fmt.Printf("FFmpeg 오류 발생: %v\n", executionErr)
		fmt.Printf("FFmpeg 오류 출력: %s\n", stderr.String())
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, fmt.Errorf("FFmpeg 오류: %v", executionErr)
	}
	fmt.Println("FFmpeg 표준 출력:\n", out.String())
	fmt.Println("FFmpeg 실행 완료. 소요시간: ", time.Now().UnixNano()/int64(time.Millisecond)-currentTimeMillis, "ms")
	currentTimeMillis = time.Now().UnixNano() / int64(time.Millisecond)

	uploadErr := uploadFileToS3("mafoo", basepath+"/output.mp4", fmt.Sprintf("recap/%s.mp4", id), s3Client)
	if uploadErr != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, uploadErr
	}
	fmt.Println("S3 업로드 완료. 소요시간: ", time.Now().UnixNano()/int64(time.Millisecond)-currentTimeMillis, "ms")
	currentTimeMillis = time.Now().UnixNano() / int64(time.Millisecond)

	os.RemoveAll(basepath)
	fmt.Println("파일 삭제 완료. 소요시간: ", time.Now().UnixNano()/int64(time.Millisecond)-currentTimeMillis, "ms")
	resultUrl := fmt.Sprintf("https://kr.object.ncloudstorage.com/mafoo/recap/%s.mp4", id)
	responseModel := ResponseModel{
		Url: resultUrl,
	}
	jsonBody, err := json.Marshal(responseModel)
	if err != nil {
		fmt.Println("JSON 변환 오류:", err)
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       `{"error": "Internal Server Error"}`,
		}, err
	}

	return events.APIGatewayProxyResponse{
		Body:       string(jsonBody),
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, nil
}

func main() {
	lambda.Start(HandleRequest)
}

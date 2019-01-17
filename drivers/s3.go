package drivers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/net"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// S3_POLICY_EXPIRE_IN_HOURS how long access rights given to other node will be valid
const S3_POLICY_EXPIRE_IN_HOURS = 24

/* S3OS S# backed object storage driver. For own storage access key and access key secret
   should be specified. To give to other nodes access to own S3 storage so called 'POST' policy
   is created. This policy is valid for S3_POLICY_EXPIRE_IN_HOURS hours.
*/
type s3OS struct {
	host               string
	region             string
	bucket             string
	awsAccessKeyID     string
	awsSecretAccessKey string
	s3svc              *s3.S3
	gsSigner           *gsSigner
	isGoogle           bool
}

type s3Session struct {
	host       string
	key        string
	policy     string
	signature  string
	credential string
	xAmzDate   string
	isGoogle   bool
}

// S3BUCKET s3 bucket owned by this node
var S3BUCKET string

func s3Host(bucket string, isGoogle bool) string {
	if isGoogle {
		return fmt.Sprintf("https://%s.storage.googleapis.com", bucket)
	}
	return fmt.Sprintf("https://%s.s3.amazonaws.com", bucket)
}

// IsOwnStorageS3 returns true if turi points to S3 bucket owned by this node
func IsOwnStorageS3(uri string) bool {
	return strings.HasPrefix(uri, s3Host(S3BUCKET, true)) ||
		strings.HasPrefix(uri, s3Host(S3BUCKET, false))
}

func newS3Session(info *net.S3OSInfo) OSSession {
	return &s3Session{
		host:       info.Host,
		key:        info.Key,
		policy:     info.Policy,
		signature:  info.Signature,
		xAmzDate:   info.XAmzDate,
		credential: info.Credential,
		isGoogle:   info.IsGoogle,
	}
}

func NewS3Driver(region, bucket, accessKey, accessKeySecret string) OSDriver {
	os := &s3OS{
		host:               s3Host(bucket, false),
		region:             region,
		bucket:             bucket,
		awsAccessKeyID:     accessKey,
		awsSecretAccessKey: accessKeySecret,
	}
	if os.awsAccessKeyID != "" {
		creds := credentials.NewStaticCredentials(os.awsAccessKeyID, os.awsSecretAccessKey, "")
		cfg := aws.NewConfig().WithRegion(os.region).WithCredentials(creds)
		os.s3svc = s3.New(session.New(), cfg)
	}
	return os
}

func (os *s3OS) NewSession(path string) OSSession {
	var policy, signature, credential, xAmzDate string
	if os.isGoogle {
		policy, signature = gsCreatePolicy(os.gsSigner, os.bucket, os.region, path)
		credential = os.gsSigner.clientEmail()
	} else {
		policy, signature, credential, xAmzDate = createPolicy(os.awsAccessKeyID,
			os.bucket, os.region, os.awsSecretAccessKey, path)
	}
	return &s3Session{
		host:       s3Host(os.bucket, os.isGoogle),
		key:        path + "/",
		policy:     policy,
		signature:  signature,
		credential: credential,
		xAmzDate:   xAmzDate,
		isGoogle:   os.isGoogle,
	}
}

func (os *s3Session) IsExternal() bool {
	return true
}

func (os *s3Session) EndSession() {
}

func (os *s3Session) SaveData(name string, data []byte) (string, error) {

	// tentativeUrl just used for logging
	tentativeURL := os.host + "/" + os.key + name
	glog.V(common.VERBOSE).Infof("Saving to S3 %s", tentativeURL)
	path, err := os.postData(name, data)
	if err != nil {
		// handle error
		glog.Errorf("Save S3 error: %v", err)
		return "", err
	}
	url := os.getAbsURL(path)

	glog.V(common.VERBOSE).Infof("Saved to S3 %s", tentativeURL)

	return url, err
}

func (os *s3Session) getAbsURL(path string) string {
	return os.host + "/" + path
}

func (os *s3Session) GetInfo() *net.OSInfo {
	return &net.OSInfo{
		StorageType: net.OSInfo_S3,
		S3Info: &net.S3OSInfo{
			Host:       os.host,
			Key:        os.key,
			Policy:     os.policy,
			Signature:  os.signature,
			Credential: os.credential,
			XAmzDate:   os.xAmzDate,
			IsGoogle:   os.isGoogle,
		},
	}
}

// if s3 storage is not our own, we are saving data into it using POST request
func (os *s3Session) postData(fileName string, buffer []byte) (string, error) {
	fileBytes := bytes.NewReader(buffer)
	fileType := http.DetectContentType(buffer)
	path, fileName := path.Split(os.key + fileName)
	fields := map[string]string{
		"acl":          "public-read",
		"Content-Type": fileType,
		"key":          path + "${filename}",
		"policy":       os.policy,
	}
	if os.isGoogle {
		fields["GoogleAccessId"] = os.credential
		fields["signature"] = os.signature
	} else {
		fields["x-amz-algorithm"] = "AWS4-HMAC-SHA256"
		fields["x-amz-credential"] = os.credential
		fields["x-amz-date"] = os.xAmzDate
		fields["x-amz-signature"] = os.signature
	}
	req, err := newfileUploadRequest(os.host, fields, fileBytes, fileName)
	if err != nil {
		glog.Error(err)
		return "", err
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		glog.Error(err)
		return "", err
	}
	body := &bytes.Buffer{}
	sz, err := body.ReadFrom(resp.Body)
	if err != nil {
		glog.Error(err)
		return "", err
	}
	resp.Body.Close()
	if sz > 0 {
		// usually there's an error at this point, so log
		glog.Error("Got response from from S3: ", body)
		return "", fmt.Errorf(body.String()) // sorta bad
	}
	return path + fileName, err
}

func makeHmac(key []byte, data []byte) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write(data)
	return hash.Sum(nil)
}

func signString(stringToSign, sregion, amzDate, secret string) string {
	date := makeHmac([]byte("AWS4"+secret), []byte(amzDate))
	region := makeHmac(date, []byte(sregion))
	service := makeHmac(region, []byte("s3"))
	credentials := makeHmac(service, []byte("aws4_request"))
	signature := makeHmac(credentials, []byte(stringToSign))
	sSignature := hex.EncodeToString(signature)
	return sSignature
}

// createPolicy returns policy, signature, xAmzCredentail and xAmzDate
func createPolicy(key, bucket, region, secret, path string) (string, string, string, string) {
	const timeFormat = "2006-01-02T15:04:05.999Z"
	const shortTimeFormat = "20060102"

	expireAt := time.Now().Add(S3_POLICY_EXPIRE_IN_HOURS * time.Hour)
	expireFmt := expireAt.UTC().Format(timeFormat)
	xAmzDate := time.Now().UTC().Format(shortTimeFormat)
	xAmzCredential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", key, xAmzDate, region)
	src := fmt.Sprintf(`{ "expiration": "%s",
    "conditions": [
      {"bucket": "%s"},
      {"acl": "public-read"},
      ["starts-with", "$Content-Type", ""],
      ["starts-with", "$key", "%s"],
      {"x-amz-algorithm": "AWS4-HMAC-SHA256"},
      {"x-amz-credential": "%s"},
      {"x-amz-date": "%sT000000Z" }
    ]
  }`, expireFmt, bucket, path, xAmzCredential, xAmzDate)
	policy := base64.StdEncoding.EncodeToString([]byte(src))
	return policy, signString(policy, region, xAmzDate, secret), xAmzCredential, xAmzDate + "T000000Z"
}

func newfileUploadRequest(uri string, params map[string]string, fData io.Reader, fileName string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for key, val := range params {
		err := writer.WriteField(key, val)
		if err != nil {
			glog.Error(err)
		}
	}
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(part, fData)

	err = writer.Close()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, err
}

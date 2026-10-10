package main

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/minio/minio-go/v7/pkg/s3utils"
)

type mediaBackendConfig struct {
	Backend, Endpoint, Region, Bucket, Prefix string
	AccessKey, SecretKey                      string
	PathStyle                                 bool
	WarnPercent                               int
}

const mediaS3AccessEnv = "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID"
const mediaS3SecretEnv = "WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY"
const mediaS3AccessFileEnv = "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID_FILE"
const mediaS3SecretFileEnv = "WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY_FILE"

func (c mediaBackendConfig) plaintextRemote() bool {
	u, err := url.Parse(c.Endpoint)
	if err != nil || c.Backend != "s3" || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// Errors name the setting only: secrets and endpoint credentials never escape.
func parseMediaBackend(getenv func(string) string) (mediaBackendConfig, error) {
	c := mediaBackendConfig{Backend: strings.TrimSpace(getenv("WHATSAPP_MEDIA_BACKEND")), Endpoint: getenv("WHATSAPP_MEDIA_S3_ENDPOINT"), Region: getenv("WHATSAPP_MEDIA_S3_REGION"), Bucket: getenv("WHATSAPP_MEDIA_S3_BUCKET"), Prefix: getenv("WHATSAPP_MEDIA_S3_PREFIX")}
	if c.Backend == "" {
		c.Backend = "local"
	}
	c.WarnPercent = 80
	if raw := getenv("WHATSAPP_MEDIA_QUOTA_WARN_PERCENT"); raw != "" {
		var err error
		c.WarnPercent, err = strconv.Atoi(raw)
		if err != nil || c.WarnPercent < 1 || c.WarnPercent > 100 {
			return c, errors.New("invalid WHATSAPP_MEDIA_QUOTA_WARN_PERCENT: expected 1..100")
		}
	}
	if c.Backend != "local" && c.Backend != "s3" {
		return c, errors.New("invalid WHATSAPP_MEDIA_BACKEND: expected local or s3")
	}
	if c.Backend == "local" {
		return c, nil
	}
	if c.Endpoint == "" {
		c.Endpoint = "https://s3.amazonaws.com"
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("invalid WHATSAPP_MEDIA_S3_ENDPOINT: expected an HTTP(S) origin without credentials")
	}
	if c.Region == "" {
		c.Region = "auto"
	}
	if s3utils.CheckValidBucketNameStrict(c.Bucket) != nil {
		return c, errors.New("invalid WHATSAPP_MEDIA_S3_BUCKET")
	}
	c.Prefix = strings.TrimSuffix(c.Prefix, "/")
	if c.Prefix == "" || len(c.Prefix) > 512 || strings.ContainsAny(c.Prefix, "\\%") || strings.ContainsFunc(c.Prefix, unicode.IsControl) {
		return c, errors.New("invalid WHATSAPP_MEDIA_S3_PREFIX: required plain path components")
	}
	for _, p := range strings.Split(c.Prefix, "/") {
		if p == "" || p == "." || p == ".." {
			return c, errors.New("invalid WHATSAPP_MEDIA_S3_PREFIX: required plain path components")
		}
	}
	c.Prefix += "/"
	c.PathStyle, err = parseBoolEnv("WHATSAPP_MEDIA_S3_FORCE_PATH_STYLE", getenv("WHATSAPP_MEDIA_S3_FORCE_PATH_STYLE"), false)
	if err != nil {
		return c, errors.New("invalid WHATSAPP_MEDIA_S3_FORCE_PATH_STYLE")
	}
	c.AccessKey, err = mediaSecret(getenv, mediaS3AccessEnv, mediaS3AccessFileEnv)
	if err != nil {
		return c, err
	}
	c.SecretKey, err = mediaSecret(getenv, mediaS3SecretEnv, mediaS3SecretFileEnv)
	return c, err
}

func mediaSecret(getenv func(string) string, key, fileKey string) (string, error) {
	value, file := getenv(key), getenv(fileKey)
	if value != "" && file != "" {
		return "", errors.New(key + ": set value or _FILE, never both")
	}
	if file != "" {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || info.Mode().Perm()&0077 != 0 {
			return "", errors.New(key + "_FILE: expected an owner-only regular file of at most 4096 bytes")
		}
		// Open with no-follow and compare identity against the inspected leaf.
		root, err := os.OpenRoot(filepath.Dir(file))
		if err != nil {
			return "", errors.New(key + "_FILE: cannot open secret")
		}
		defer func() { _ = root.Close() }()
		f, err := root.OpenFile(filepath.Base(file), os.O_RDONLY, 0)
		if err != nil {
			return "", errors.New(key + "_FILE: cannot open secret")
		}
		defer func() { _ = f.Close() }()
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return "", errors.New(key + "_FILE: secret changed while opening")
		}
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || len(data) > 4096 {
			return "", errors.New(key + "_FILE: cannot read secret")
		}
		value = strings.TrimSpace(string(data))
	}
	if value == "" || len(value) > 4096 || strings.ContainsFunc(value, unicode.IsControl) {
		return "", errors.New(key + ": required bounded credential")
	}
	return value, nil
}

package dataproxy

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/ioutils"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/flyteorg/flyteadmin/pkg/config"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/flyteorg/flytestdlib/storage"
	"github.com/flyteorg/stow"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/service"
)

type Service struct {
	service.DataProxyServiceServer

	cfg           config.DataProxyConfig
	dataStore     *storage.DataStore
	shardSelector ioutils.ShardSelector
}

// CreateUploadLocation creates a temporary signed url to allow callers to upload content.
func (s Service) CreateUploadLocation(ctx context.Context, req *service.CreateUploadLocationRequest) (
	*service.CreateUploadLocationResponse, error) {

	if len(req.Project) == 0 || len(req.Domain) == 0 {
		return nil, fmt.Errorf("prjoect and domain are required parameters")
	}

	if len(req.ContentMd5) == 0 {
		return nil, fmt.Errorf("content_md5 is a required parameter")
	}

	if expiresIn := req.ExpiresIn; expiresIn != nil {
		if !expiresIn.IsValid() {
			return nil, fmt.Errorf("expiresIn [%v] is invalid", expiresIn)
		}

		if expiresIn.AsDuration() > s.cfg.Upload.MaxExpiresIn.Duration {
			return nil, fmt.Errorf("expiresIn [%v] cannot exceed max allowed expiration [%v]",
				expiresIn.AsDuration().String(), s.cfg.Upload.MaxExpiresIn.String())
		}
	} else {
		req.ExpiresIn = durationpb.New(s.cfg.Upload.MaxExpiresIn.Duration)
	}

	if len(req.Filename) == 0 {
		req.Filename = rand.String(s.cfg.Upload.DefaultFileNameLength)
	}

	md5 := base64.StdEncoding.EncodeToString(req.ContentMd5)
	urlSafeMd5 := base32.StdEncoding.EncodeToString(req.ContentMd5)

	storagePath, err := createShardedStorageLocation(ctx, s.shardSelector, s.dataStore, s.cfg.Upload,
		req.Project, req.Domain, urlSafeMd5, req.Filename)
	if err != nil {
		return nil, err
	}

	resp, err := s.dataStore.CreateSignedURL(ctx, storagePath, storage.SignedURLProperties{
		Scope:      stow.ClientMethodPut,
		ExpiresIn:  req.ExpiresIn.AsDuration(),
		ContentMD5: md5,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create a signed url. Error: %w", err)
	}

	return &service.CreateUploadLocationResponse{
		SignedUrl: resp.URL.String(),
		NativeUrl: storagePath.String(),
		ExpiresAt: timestamppb.New(time.Now().Add(req.ExpiresIn.AsDuration())),
	}, nil
}

// CreateDownloadLocation creates a temporary signed url to allow callers to download content.
func (s Service) CreateDownloadLocation(ctx context.Context, req *service.CreateDownloadLocationRequest) (
	*service.CreateDownloadLocationResponse, error) {

	if err := s.validateCreateDownloadLocationRequest(req); err != nil {
		return nil, err
	}

	resp, err := s.dataStore.CreateSignedURL(ctx, storage.DataReference(req.NativeUrl), storage.SignedURLProperties{
		Scope:     stow.ClientMethodGet,
		ExpiresIn: req.ExpiresIn.AsDuration(),
	})

	if err != nil {
		return nil, fmt.Errorf("failed to create a signed url. Error: %w", err)
	}

	return &service.CreateDownloadLocationResponse{
		SignedUrl: resp.URL.String(),
		ExpiresAt: timestamppb.New(time.Now().Add(req.ExpiresIn.AsDuration())),
	}, nil
}

func (s Service) validateCreateDownloadLocationRequest(req *service.CreateDownloadLocationRequest) error {
	if expiresIn := req.ExpiresIn; expiresIn != nil {
		if !expiresIn.IsValid() {
			return fmt.Errorf("expiresIn [%v] is invalid", expiresIn)
		}

		if expiresIn.AsDuration() < 0 {
			return fmt.Errorf("expiresIn [%v] should not less than 0",
				expiresIn.AsDuration().String())
		} else if expiresIn.AsDuration() > s.cfg.Download.MaxExpiresIn.Duration {
			return fmt.Errorf("expiresIn [%v] cannot exceed max allowed expiration [%v]",
				expiresIn.AsDuration().String(), s.cfg.Download.MaxExpiresIn.String())
		}
	} else {
		req.ExpiresIn = durationpb.New(s.cfg.Download.MaxExpiresIn.Duration)
	}

	if _, err := url.Parse(req.NativeUrl); err != nil {
		return fmt.Errorf("failed to parse native_url [%v]",
			req.NativeUrl)
	}

	return nil
}

// createShardedStorageLocation creates a location in storage destination to maximize read/write performance in most
// block stores. The final location should look something like: s3://<my bucket>/<shard length>/<file name>
func createShardedStorageLocation(ctx context.Context, shardSelector ioutils.ShardSelector, store *storage.DataStore,
	cfg config.DataProxyUploadConfig, keyParts ...string) (storage.DataReference, error) {
	keySuffixArr := make([]string, 0, 4)
	if len(cfg.StoragePrefix) > 0 {
		keySuffixArr = append(keySuffixArr, cfg.StoragePrefix)
	}

	keySuffixArr = append(keySuffixArr, keyParts...)
	prefix, err := shardSelector.GetShardPrefix(ctx, []byte(strings.Join(keySuffixArr, "/")))
	if err != nil {
		return "", err
	}

	storagePath, err := store.ConstructReference(ctx, store.GetBaseContainerFQN(ctx),
		append([]string{prefix}, keySuffixArr...)...)
	if err != nil {
		return "", fmt.Errorf("failed to construct datastore reference. Error: %w", err)
	}

	return storagePath, nil
}

func NewService(cfg config.DataProxyConfig, dataStore *storage.DataStore) (Service, error) {
	// Context is not used in the constructor. Should ideally be removed.
	selector, err := ioutils.NewBase36PrefixShardSelector(context.TODO())
	if err != nil {
		return Service{}, err
	}

	return Service{
		cfg:           cfg,
		dataStore:     dataStore,
		shardSelector: selector,
	}, nil
}

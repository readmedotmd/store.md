package s3store

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/ory/dockertest/v3"

	storemd "github.com/readmedotmd/store.md"
)

var minioClient *minio.Client

const testBucket = "test-bucket"

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// startMinioDocker starts minio using dockertest. Returns a cleanup function.
func startMinioDocker() (endpoint string, cleanup func(), err error) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return "", nil, fmt.Errorf("could not construct pool: %w", err)
	}
	if err := pool.Client.Ping(); err != nil {
		return "", nil, fmt.Errorf("could not ping docker: %w", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "minio/minio",
		Tag:        "latest",
		Cmd:        []string{"server", "/data"},
		Env: []string{
			"MINIO_ROOT_USER=minioadmin",
			"MINIO_ROOT_PASSWORD=minioadmin",
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("could not start minio: %w", err)
	}

	endpoint = fmt.Sprintf("localhost:%s", resource.GetPort("9000/tcp"))
	cleanup = func() { pool.Purge(resource) }
	return endpoint, cleanup, nil
}

// startMinioBinary starts minio as a local binary (fallback when Docker is unavailable).
func startMinioBinary() (endpoint string, cleanup func(), err error) {
	minioBin := "/tmp/minio"
	if _, err := os.Stat(minioBin); err != nil {
		return "", nil, fmt.Errorf("minio binary not found at %s: %w", minioBin, err)
	}

	port, err := freePort()
	if err != nil {
		return "", nil, fmt.Errorf("could not get free port: %w", err)
	}

	dataDir, err := os.MkdirTemp("", "minio-data-*")
	if err != nil {
		return "", nil, fmt.Errorf("could not create temp dir: %w", err)
	}

	cmd := exec.Command(minioBin, "server", dataDir, "--address", fmt.Sprintf(":%d", port))
	cmd.Env = append(os.Environ(),
		"MINIO_ROOT_USER=minioadmin",
		"MINIO_ROOT_PASSWORD=minioadmin",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		os.RemoveAll(dataDir)
		return "", nil, fmt.Errorf("could not start minio: %w", err)
	}

	endpoint = fmt.Sprintf("127.0.0.1:%d", port)
	cleanup = func() {
		cmd.Process.Kill()
		cmd.Wait()
		os.RemoveAll(dataDir)
	}
	return endpoint, cleanup, nil
}

func TestMain(m *testing.M) {
	// Try Docker first, fall back to local binary.
	endpoint, cleanup, err := startMinioDocker()
	if err != nil {
		log.Printf("Docker unavailable (%v), falling back to local minio binary", err)
		endpoint, cleanup, err = startMinioBinary()
		if err != nil {
			log.Fatalf("Could not start minio: %s", err)
		}
	}
	defer cleanup()

	// Configure minio client.
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("minioadmin", "minioadmin", ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("Could not create minio client: %s", err)
	}

	// Wait for minio to be ready.
	ready := false
	for i := 0; i < 50; i++ {
		_, err := minioClient.ListBuckets(context.Background())
		if err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		log.Fatalf("Minio did not become ready in time")
	}

	// Create a test bucket.
	err = minioClient.MakeBucket(context.Background(), testBucket, minio.MakeBucketOptions{})
	if err != nil {
		log.Fatalf("Could not create bucket: %s", err)
	}

	code := m.Run()
	os.Exit(code)
}

func TestS3Store(t *testing.T) {
	counter := 0
	storemd.RunStoreTests(t, func(t *testing.T) storemd.Store {
		// Use a unique prefix per test to provide isolation.
		counter++
		prefix := fmt.Sprintf("test-%d/", counter)
		return New(minioClient, testBucket, prefix)
	})
}

func TestS3Store_SetIfNotExists(t *testing.T) {
	counter := 0
	storemd.RunSetIfNotExistsTests(t, func(t *testing.T) storemd.Store {
		counter++
		prefix := fmt.Sprintf("test-sine-%d/", counter)
		return New(minioClient, testBucket, prefix)
	})
}

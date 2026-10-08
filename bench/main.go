// Command bench drives the same S3 workload through aws-sdk-go-v2 against any endpoint, so ss33 and other
// S3-compatible servers are measured with one client and one set of requests.
//
//	go run . -ep http://localhost:9000 -name ss33 [-json results.jsonl]
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var ctx = context.Background()

type result struct {
	Server string  `json:"server"`
	Test   string  `json:"test"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Error  string  `json:"error,omitempty"`
}

func main() {
	ep := flag.String("ep", "http://localhost:9000", "S3 endpoint")
	name := flag.String("name", "server", "label for the results")
	ak := flag.String("access-key", "minioadmin", "access key")
	sk := flag.String("secret-key", "minioadmin", "secret key")
	nList := flag.Int("list", 20000, "objects for the listing test")
	out := flag.String("json", "", "append results as JSON lines to this file")
	flag.Parse()

	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: ep, UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(*ak, *sk, "")})
	var results []result
	record := func(test string, value float64, unit string, err error) {
		r := result{Server: *name, Test: test, Value: value, Unit: unit}
		if err != nil {
			r.Error = err.Error()
			fmt.Printf("%-36s error: %v\n", test, err)
		} else {
			fmt.Printf("%-36s %10.1f %s\n", test, value, unit)
		}
		results = append(results, r)
	}

	b := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &b}); err != nil {
		fmt.Fprintln(os.Stderr, "create bucket:", err)
		os.Exit(1)
	}
	small := random(4096)
	key := func(i int) *string { return aws.String(fmt.Sprintf("small/%05d", i)) }

	record(ops("PUT 4 KiB, 16 concurrent", 2000, 16, func(i int) error {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: key(i), Body: bytes.NewReader(small)})
		return err
	}))
	record(ops("GET 4 KiB, 16 concurrent", 2000, 16, func(i int) error {
		r, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: key(i)})
		if err != nil {
			return err
		}
		defer r.Body.Close()
		_, err = io.Copy(io.Discard, r.Body)
		return err
	}))
	record(ops("HEAD, 16 concurrent", 2000, 16, func(i int) error {
		_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: key(i)})
		return err
	}))

	big := random(256 << 20)
	record(throughput("PUT 256 MiB", len(big), func() error {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: aws.String("big.bin"), Body: bytes.NewReader(big)})
		return err
	}))
	record(throughput("GET 256 MiB", len(big), func() error {
		r, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: aws.String("big.bin")})
		if err != nil {
			return err
		}
		defer r.Body.Close()
		_, err = io.Copy(io.Discard, r.Body)
		return err
	}))

	// A resumable browser upload: 32 x 8 MiB parts in parallel, ListParts to resume, then Complete.
	mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b, Key: aws.String("mp.bin")})
	if err != nil {
		record("UploadPart 32 x 8 MiB", 0, "MiB/s", err)
	} else {
		parts := make([]types.CompletedPart, 32)
		record(throughput("UploadPart 32 x 8 MiB, 32 concurrent", len(big), func() error {
			var wg sync.WaitGroup
			errs := make([]error, 32)
			for i := range 32 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &b, Key: aws.String("mp.bin"), UploadId: mp.UploadId,
						PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(big[i*8<<20 : (i+1)*8<<20])})
					if err != nil {
						errs[i] = err
						return
					}
					parts[i] = types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag}
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			return nil
		}))
		record(latency("ListParts, 32 parts", func() error {
			_, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: &b, Key: aws.String("mp.bin"), UploadId: mp.UploadId})
			return err
		}))
		record(latency("CompleteMultipartUpload, 256 MiB", func() error {
			_, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b, Key: aws.String("mp.bin"), UploadId: mp.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
			return err
		}))
	}

	lb := b + "-list"
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &lb}); err != nil {
		record("PUT 0 B", 0, "ops/s", err)
	} else {
		record(ops(fmt.Sprintf("PUT 0 B x %d, 32 concurrent", *nList), *nList, 32, func(i int) error {
			_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &lb, Key: aws.String(fmt.Sprintf("d%03d/obj-%06d", i%100, i)), Body: bytes.NewReader(nil)})
			return err
		}))
		record(latency(fmt.Sprintf("ListObjectsV2, all %d keys", *nList), func() error {
			p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &lb})
			n := 0
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return err
				}
				n += len(page.Contents)
			}
			if n != *nList {
				return fmt.Errorf("listed %d keys, want %d", n, *nList)
			}
			return nil
		}))
		record(latency("ListObjectsV2, one prefix page", func() error {
			_, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &lb, Prefix: aws.String("d042/"), MaxKeys: aws.Int32(50)})
			return err
		}))
	}

	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		for _, r := range results {
			enc.Encode(r)
		}
	}
}

// ops runs n calls over conc workers and reports operations per second.
func ops(test string, n, conc int, fn func(i int) error) (string, float64, string, error) {
	var next int64 = -1
	var firstErr atomic.Value
	start := time.Now()
	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= n || firstErr.Load() != nil {
					return
				}
				if err := fn(i); err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err, _ := firstErr.Load().(error); err != nil {
		return test, 0, "ops/s", err
	}
	return test, float64(n) / time.Since(start).Seconds(), "ops/s", nil
}

func throughput(test string, bytes int, fn func() error) (string, float64, string, error) {
	start := time.Now()
	if err := fn(); err != nil {
		return test, 0, "MiB/s", err
	}
	return test, float64(bytes) / (1 << 20) / time.Since(start).Seconds(), "MiB/s", nil
}

func latency(test string, fn func() error) (string, float64, string, error) {
	start := time.Now()
	if err := fn(); err != nil {
		return test, 0, "ms", err
	}
	return test, float64(time.Since(start).Microseconds()) / 1000, "ms", nil
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

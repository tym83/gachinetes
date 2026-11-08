package main

import (
	"testing"
	"time"
)

func TestReplicationFromSecret(t *testing.T) {
	r, err := fromSecret(map[string][]byte{
		"bucket": []byte("s3://tapes/gym\n"), "s3Endpoint": []byte("https://minio:9000"),
		"encryptionRecipient": []byte("age1aaa\nage1bbb"), "retention": []byte("720h"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.to != "s3://tapes/gym" || r.region != "us-east-1" || len(r.recipients) != 2 || r.retention != 720*time.Hour ||
		r.interval != 30*time.Second || r.baseEvery != 24*time.Hour {
		t.Fatalf("%+v", r)
	}
	if _, err := fromSecret(map[string][]byte{"s3Endpoint": []byte("https://minio:9000")}); err == nil {
		t.Fatal("no bucket accepted")
	}
	if _, err := fromSecret(map[string][]byte{"bucket": []byte("s3://a"), "interval": []byte("soon")}); err == nil {
		t.Fatal("a bad interval accepted")
	}
	if digest(map[string][]byte{"a": []byte("1"), "b": []byte("2")}) != digest(map[string][]byte{"b": []byte("2"), "a": []byte("1")}) {
		t.Fatal("digest depends on order")
	}
}

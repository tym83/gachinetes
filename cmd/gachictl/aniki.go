package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/tym83/gachinetes/pkg/aniki"
)

func (a *app) anikiCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "aniki [-f FILE | why KIND/NAME]",
		Short: "The gym's mentor: reviews manifests, and talks a failure through",
		Long: `aniki -f FILE reviews the workloads of a manifest.
aniki why KIND/NAME tells what gachictl why tells, and, when
GACHI_ANIKI_URL names an OpenAI-compatible endpoint (with GACHI_ANIKI_MODEL
and, if needed, GACHI_ANIKI_KEY), what to do about it.`,
		Args: cobra.RangeArgs(0, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == "why" {
				t, err := a.target(args[1:], false)
				if err != nil {
					return err
				}
				out, err := a.why(cmd.Context(), t)
				if err != nil {
					return err
				}
				fmt.Print(out)
				if os.Getenv("GACHI_ANIKI_URL") == "" {
					return nil
				}
				advice, err := mentor(cmd.Context(), out)
				if err != nil {
					return fmt.Errorf("Aniki could not be reached: %w", err)
				}
				fmt.Printf("\n♂ Aniki says:\n%s\n", advice)
				return nil
			}
			if file == "" {
				return fmt.Errorf("-f FILE, or why KIND/NAME")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			n := 0
			for _, doc := range bytes.Split(raw, []byte("\n---")) {
				u := &unstructured.Unstructured{}
				if yaml.Unmarshal(doc, &u.Object) != nil || len(u.Object) == 0 {
					continue
				}
				for _, f := range aniki.Review(u) {
					fmt.Println(f)
					n++
				}
			}
			if n == 0 {
				fmt.Println("♂ Aniki nods. Nothing to say.")
			}
			return nil
		},
	}
	c.Flags().StringVarP(&file, "filename", "f", "", "the manifest to review")
	return c
}

// mentor asks a language model, as Aniki, what to do about a failure.
func mentor(ctx context.Context, why string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": os.Getenv("GACHI_ANIKI_MODEL"),
		"messages": []map[string]string{
			{"role": "system", "content": "You are Aniki, a kind and exact senior Kubernetes engineer. Given what is wrong with an object and what led to it, say in at most five short lines what to check and what to change. No preamble."},
			{"role": "user", "content": why},
		},
	})
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(os.Getenv("GACHI_ANIKI_URL"), "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("GACHI_ANIKI_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: no answer", resp.Status)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

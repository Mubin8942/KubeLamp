package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Run a CronJob now: create a Job from its jobTemplate, like
// `kubectl create job <name>-manual-<time> --from=cronjob/<name>`.
// The Job is named <cronjob>-manual-<YYYYMMDD-HHMMSS> (UTC); Kubernetes names its pods
// <job>-<5 random characters>.

// The CronJob controller keeps CronJob names to 52 characters so the Jobs it creates fit
// the 63-character label limit; manual Jobs use the same budget.
const maxJobName = 52

func manualJobName(cronJob string, now time.Time) string {
	suffix := "-manual-" + now.UTC().Format("20060102-150405")
	base := cronJob
	if max := maxJobName - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-.")
	}
	return base + suffix
}

type cronJobObj struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		JobTemplate struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec json.RawMessage `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

// jobFromCronJob builds the Job manifest: the template's labels, annotations and spec,
// owned by the CronJob (so it is cleaned up with it), marked as started by hand.
func jobFromCronJob(cj cronJobObj, name string) ([]byte, error) {
	if len(cj.Spec.JobTemplate.Spec) == 0 || string(cj.Spec.JobTemplate.Spec) == "null" {
		return nil, errors.New("this CronJob has no job template")
	}
	ann := map[string]string{}
	for k, v := range cj.Spec.JobTemplate.Metadata.Annotations {
		ann[k] = v
	}
	ann["cronjob.kubernetes.io/instantiate"] = "manual" // what kubectl sets
	ann["kubelamp.io/triggered-by"] = "KubeLamp"
	apiVersion := cj.APIVersion
	if apiVersion == "" {
		apiVersion = "batch/v1"
	}
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   cj.Metadata.Namespace,
			"labels":      cj.Spec.JobTemplate.Metadata.Labels,
			"annotations": ann,
			"ownerReferences": []map[string]any{{
				"apiVersion": apiVersion, "kind": "CronJob", "name": cj.Metadata.Name, "uid": cj.Metadata.UID,
				"controller": true, "blockOwnerDeletion": true,
			}},
		},
		"spec": cj.Spec.JobTemplate.Spec,
	}
	return json.Marshal(job)
}

// runCronJob creates the Job and returns its name.
func runCronJob(ctx context.Context, c *Cluster, ns, name string) (string, error) {
	if ns == "" || name == "" {
		return "", errors.New("namespace and name required")
	}
	b, err := c.client.send(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+url.PathEscape(ns)+"/cronjobs/"+url.PathEscape(name), nil, nil, "")
	if err != nil {
		return "", err
	}
	var cj cronJobObj
	if err := json.Unmarshal(b, &cj); err != nil {
		return "", err
	}
	if cj.Metadata.UID == "" {
		return "", errors.New("CronJob not found")
	}
	jobName := manualJobName(cj.Metadata.Name, time.Now())
	body, err := jobFromCronJob(cj, jobName)
	if err != nil {
		return "", err
	}
	if _, err := c.client.send(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+url.PathEscape(ns)+"/jobs", nil, body, "application/json"); err != nil {
		return "", err
	}
	return jobName, nil
}

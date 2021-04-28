// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/run/v1"
)

func main() {
	// Agenda:
	// * How to check if a Cloud Run service exists
	// * How to create a new Cloud Run service
	// * How to wait for the service to become "ready"
	// * How to give IAM permissions to make the service public
	// * How to release a new Revision and split traffic v1 and v2
	// * How to delete a service

	// To authenticate to Google Cloud APIs:
	// - On your laptop:
	//     - run "gcloud auth application-default login"
	// - On GCP compute environment
	//     - automatic authentication: a service account will be picked up from
	//       the environment
	// - Anywhere else:
	//     - create a service account key and set its path via
	//       GOOGLE_APPLICATION_CREDENTIALS env var

	name := "hello"
	region := "us-central1"
	project := "ahmetb-demo"
	c, err := client(region)
	if err != nil {
		log.Fatal(fmt.Errorf("failed to initialize client: %w", err))
	}

	// check if Cloud Run service exists.
	exists, err := serviceExists(c, region, project, name)
	panicIfErr(err)
	log.Printf("service exists?: %v", exists)

	// deploying the first revision (v1) is quite easy.
	// check out the YAML tab of your service and reconstruct it in code.
	svc := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name: name,
		},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Metadata: &run.ObjectMeta{Name: name + "-v1"},
				Spec: &run.RevisionSpec{
					Containers: []*run.Container{
						{
							Image: "gcr.io/google-samples/hello-app:1.0",
						},
					},
				},
			},
		},
	}
	_, err = c.Namespaces.Services.Create("namespaces/"+project, svc).Do()
	panicIfErr(err)
	log.Printf("service create call completed")
	// at this point, the service might not be ready.
	// to check if the Revision works correctly or not,
	// see the status field on the Service object by querying it

	// wait for revision to become ready
	log.Printf("waiting for service to become ready")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*120)
	defer cancel()

	err = waitForReady(ctx, c, region, project, name, "Ready")
	panicIfErr(err)
	log.Printf("service is ready and serving traffic!")

	// give service public access via IAM bindings.
	// we'll need to use the non-regional API endpoint with this.
	gc, err := run.NewService(context.TODO())
	panicIfErr(err)
	_, err = gc.Projects.Locations.Services.SetIamPolicy(
		fmt.Sprintf("projects/%s/locations/%s/services/%s", project, region, name),
		&run.SetIamPolicyRequest{
			Policy: &run.Policy{Bindings: []*run.Binding{{
				Members: []string{"allUsers"},
				Role:    "roles/run.invoker",
			}}},
		},
	).Do()
	panicIfErr(err)

	// print the service URL by re-querying the service because the
	// url becomes available on the object after the Create() call
	svc, err = getService(c, region, project, name)
	panicIfErr(err)
	log.Printf("service is deployed at: %s", svc.Status.Address.Url)

	// to deploy a new revision, we need a fresh Service object from
	// the API. this way we can make use of the builtin optimistic concurrency
	// control in the API and ensure we are not accidentally overwriting a
	// coinciding update happening at the same time (write race).
	svc, err = getService(c, region, project, name)
	panicIfErr(err)
	svc.Spec.Template.Metadata.Name = name + "-v2"
	svc.Spec.Template.Spec.Containers[0].Image = "gcr.io/google-samples/hello-app:2.0"
	svc.Spec.Template.Spec.Containers[0].Env = []*run.EnvVar{{Name: "FOO", Value: "bar"}}
	svc.Spec.Template.Spec.Containers[0].Resources.Limits = map[string]string{
		"cpu":    "2",
		"memory": "1Gi"}
	// let's split traffic as v1=90% v2=10%
	svc.Spec.Traffic = []*run.TrafficTarget{{
		RevisionName: name + "-v1",
		Percent:      90,
	}, {
		RevisionName: name + "-v2",
		Percent:      10,
	}}
	_, err = c.Namespaces.Services.ReplaceService(fmt.Sprintf("namespaces/%s/services/%s", project, name), svc).Do()
	panicIfErr(err)
	log.Printf("deployed an update, might not be ready")

	// wait for the service to become ready and start serving the route changes
	err = waitForReady(ctx, c, region, project, name, "Ready")
	panicIfErr(err)
	err = waitForReady(ctx, c, region, project, name, "RoutesReady")
	panicIfErr(err)
	log.Printf("updated service is ready and serving with traffic split")

	// delete the service.
	op, err := c.Namespaces.Services.Delete(fmt.Sprintf("namespaces/%s/services/%s", project, name)).Do()
	panicIfErr(err)
	// TODO: you can check for op.Status="Success" here, and the deletion
	// will happen asynchronously (you can query the Service and see its Ready status)
	// and it will eventually disappear from the API (serviceExists will return false).
	// Not implementing that here for brevity.
	_ = op
	log.Printf("deleted service")
}

func serviceExists(c *run.APIService, region, project, name string) (bool, error) {
	_, err := c.Namespaces.Services.Get(fmt.Sprintf("namespaces/%s/services/%s", project, name)).Do()
	if err == nil {
		return true, nil
	}
	// not all errors indicate service does not exist, look for 404 status code
	v, ok := err.(*googleapi.Error)
	if !ok {
		return false, fmt.Errorf("failed to query service: %w", err)
	}
	if v.Code == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("unexpected status code=%d from get service call: %w", v.Code, err)
}

func getService(c *run.APIService, region, project, name string) (*run.Service, error) {
	return c.Namespaces.Services.Get(fmt.Sprintf("namespaces/%s/services/%s", project, name)).Do()
}

func waitForReady(ctx context.Context, c *run.APIService, region, project, name, condition string) error {
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			svc, err := getService(c, region, project, name)
			if err != nil {
				return fmt.Errorf("failed to query service for readiness: %w", err)
			}
			for _, c := range svc.Status.Conditions {
				if c.Type == condition {
					if c.Status == "True" {
						return nil
					} else if c.Status == "False" {
						return fmt.Errorf("service could not become %q (status:%s) (reason:%s) %s",
							condition, c.Status, c.Reason, c.Message)
					}
				}
			}
		}
	}
}

func client(region string) (*run.APIService, error) {
	return run.NewService(context.TODO(),
		option.WithEndpoint(fmt.Sprintf("https://%s-run.googleapis.com", region)))
}

func panicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}

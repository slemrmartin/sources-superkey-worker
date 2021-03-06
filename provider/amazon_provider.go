package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/redhatinsights/sources-superkey-worker/amazon"
	l "github.com/redhatinsights/sources-superkey-worker/logger"
	"github.com/redhatinsights/sources-superkey-worker/superkey"
)

// AmazonProvider struct for implementing the Amazon Provider interface
type AmazonProvider struct {
	Client *amazon.Client
}

// ForgeApplication transforms a superkey request with the amazon provider into a list
// of resources required for the application, specified by the request.
// returns: the new forged application payload with info on what was processed, in case something went wrong.
func (a *AmazonProvider) ForgeApplication(request *superkey.CreateRequest) (*superkey.ForgedApplication, error) {
	f := &superkey.ForgedApplication{
		StepsCompleted: make(map[string]map[string]string),
		Request:        request,
		Client:         a,
		GUID:           generateGUID(),
	}

	for _, step := range request.SuperKeySteps {
		switch step.Name {
		case "s3":
			name := fmt.Sprintf("%v-bucket-%v", getShortName(f.Request.ApplicationType), f.GUID)
			l.Log.Infof("Creating S3 bucket: %v", name)

			err := a.Client.CreateS3Bucket(name)
			if err != nil {
				l.Log.Errorf("Failed to create S3 bucket %v, rolling back superkey request %v", name, f.Request)
				return f, err
			}

			f.MarkCompleted("s3", map[string]string{"output": name})
			l.Log.Infof("Successfully created S3 bucket %v", name)

		case "policy":
			name := fmt.Sprintf("%v-policy-%v", getShortName(f.Request.ApplicationType), f.GUID)
			payload := substiteInPayload(step.Payload, f, step.Substitutions)
			l.Log.Infof("Creating Policy %v", name)

			arn, err := a.Client.CreatePolicy(name, payload)
			if err != nil {
				l.Log.Error("Failed to create Policy %v, rolling back superkey request %v", name, f.Request)
				return f, err
			}

			f.MarkCompleted("policy", map[string]string{"output": *arn})
			l.Log.Infof("Successfully created policy %v", name)

		case "role":
			name := fmt.Sprintf("%v-role-%v", getShortName(f.Request.ApplicationType), f.GUID)
			payload := substiteInPayload(step.Payload, f, step.Substitutions)
			l.Log.Infof("Creating Role %v", name)

			roleArn, err := a.Client.CreateRole(name, payload)
			if err != nil {
				l.Log.Error("Failed to create Role %v, rolling back superkey request %v", name, f.Request)
				return f, err
			}

			// Store the Role ARN since that is what we need to return for the Authentication object.
			f.MarkCompleted("role", map[string]string{"output": name, "arn": *roleArn})
			l.Log.Infof("Successfully created role %v", name)

		case "bind_role":
			roleName := f.StepsCompleted["role"]["output"]
			policyArn := f.StepsCompleted["policy"]["output"]
			l.Log.Infof("Binding role %v with policy arn %v", roleName, policyArn)

			err := a.Client.BindPolicyToRole(policyArn, roleName)
			if err != nil {
				l.Log.Errorf("Failed to bind policy %v to role arn %v, rolling back superkey request %v", policyArn, roleName, f.Request)
				return f, err
			}

			f.MarkCompleted("bind_role", map[string]string{})
			l.Log.Infof("Successfully bound role %v to policy %v", roleName, policyArn)

		default:
			l.Log.Infof("%v not implemented yet!", step.Name)
		}
	}

	// Set the username to the role ARN since that is what is needed for this provider.
	username := f.StepsCompleted["role"]["arn"]
	appType := path.Base(f.Request.ApplicationType)
	// Create the payload struct
	f.CreatePayload(&username, nil, &appType)

	return f, nil
}

// generateGUID() generates a short guid for resources
func generateGUID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// getShortName(string) generates a name off of the application type
func getShortName(name string) string {
	return fmt.Sprintf("redhat-%s", path.Base(name))
}

func substiteInPayload(payload string, f *superkey.ForgedApplication, substitutions map[string]string) string {
	// these are some special case substitutions, where `get_account` implies we need to fetch
	// the account from the payload, and s3 is just the output from the s3 step
	for name, sub := range substitutions {
		switch sub {
		case "get_account":
			accountNumber := f.Request.Extra["account"]
			payload = strings.Replace(payload, name, accountNumber, -1)
		case "s3":
			s3name := f.StepsCompleted["s3"]["output"]
			payload = strings.Replace(payload, name, s3name, -1)
		}
	}

	return payload
}

// TearDown - provides amazon logic for tearing down a supported application
// returns: error
//
// Basically the StepsCompleted field keeps track of what parts of the forge operation
// went smoothly, and we just go through them in reverse and handle them.
func (a *AmazonProvider) TearDown(f *superkey.ForgedApplication) []error {
	errors := make([]error, 0)

	// -----------------
	// unbind the role first (if it happened) so we can cleanly delete the policy and the role.
	// -----------------
	if f.StepsCompleted["bind_role"] != nil {
		policyArn := f.StepsCompleted["policy"]["output"]
		role := f.StepsCompleted["role"]["output"]

		err := a.Client.UnBindPolicyToRole(policyArn, role)
		if err != nil {
			l.Log.Warnf("Failed to unbind policy %v from role %v", policyArn, role)
			errors = append(errors, err)
		}

		l.Log.Infof("Un-bound policy %v from role %v", policyArn, role)
	}

	// -----------------
	// role/policy can be deleted independently of each other.
	// -----------------
	if f.StepsCompleted["policy"] != nil {
		policyArn := f.StepsCompleted["policy"]["output"]

		err := a.Client.DestroyPolicy(policyArn)
		if err != nil {
			l.Log.Warnf("Failed to destroy policy %v", policyArn)
			errors = append(errors, err)
		}

		l.Log.Infof("Destroyed policy %v", policyArn)
	}

	if f.StepsCompleted["role"] != nil {
		roleName := f.StepsCompleted["role"]["output"]

		err := a.Client.DestroyRole(roleName)
		if err != nil {
			l.Log.Warnf("Failed to destroy role %v", roleName)
			errors = append(errors, err)
		}

		l.Log.Infof("Destroyed role %v", roleName)
	}

	// -----------------
	// s3 bucket can probably be deleted earlier, but leave it to last just in case
	// other things depend on it.
	// -----------------
	if f.StepsCompleted["s3"] != nil {
		bucket := f.StepsCompleted["s3"]["output"]

		err := a.Client.DestroyS3Bucket(bucket)
		if err != nil {
			l.Log.Warnf("Failed to destroy s3 bucket %v", bucket)
			errors = append(errors, err)
		}

		l.Log.Infof("Destroyed s3 bucket %v", bucket)
	}

	return errors
}

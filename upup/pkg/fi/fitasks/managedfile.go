/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fitasks

import (
	"bytes"
	"fmt"
	"os"

	"k8s.io/kops/pkg/featureflag"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kops/pkg/acls"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/util/pkg/vfs"
)

// +kops:fitask
type ManagedFile struct {
	Name      *string
	Lifecycle fi.Lifecycle

	// Base is the root location of the store for the managed file
	Base *string

	// Location is the relative path of the managed file
	Location *string

	Contents fi.Resource

	// PublicACL controls whether the _object_ has an ACL which grants world-readable status.
	// Note that the _bucket_ may itself have a grant for world-readable; that is separate.
	PublicACL *bool
}

func (e *ManagedFile) Find(c *fi.CloudupContext) (*ManagedFile, error) {
	managedFiles, err := getBasePath(c, e)
	if err != nil {
		return nil, err
	}

	location := fi.ValueOf(e.Location)
	if location == "" {
		return nil, nil
	}

	filePath := managedFiles.Join(location)

	existingData, err := filePath.ReadFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	actual := &ManagedFile{
		Name:     e.Name,
		Base:     e.Base,
		Location: e.Location,
		Contents: fi.NewBytesResource(existingData),
	}

	if s3file, ok := filePath.(*vfs.S3Path); ok {
		public, err := s3file.IsPublic()
		if err != nil {
			return nil, err
		}
		actual.PublicACL = &public

		if e.PublicACL == nil {
			e.PublicACL = fi.PtrTo(false)
		}
	}

	if memfsfile, ok := filePath.(*vfs.MemFSPath); ok {
		public, err := memfsfile.IsPublic()
		if err != nil {
			return nil, err
		}
		actual.PublicACL = &public

		if e.PublicACL == nil {
			e.PublicACL = fi.PtrTo(false)
		}
	}

	// Avoid spurious changes
	actual.Lifecycle = e.Lifecycle

	return actual, nil
}

func (e *ManagedFile) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

func (s *ManagedFile) CheckChanges(a, e, changes *ManagedFile) error {
	if a != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
	}
	if e.Contents == nil {
		return field.Required(field.NewPath("Contents"), "")
	}
	return nil
}

func (e *ManagedFile) getACL(c *fi.CloudupContext, p vfs.Path) (vfs.ACL, error) {
	ctx := c.Context()

	var acl vfs.ACL
	if fi.ValueOf(e.PublicACL) {
		switch p := p.(type) {
		case *vfs.S3Path:
			acl = &vfs.S3Acl{
				RequestACL: fi.PtrTo("public-read"),
			}
		case *vfs.MemFSPath:
			if !p.IsClusterReadable() {
				return nil, fmt.Errorf("the %q path is intended for use in tests", p.Path())
			}
			acl = &vfs.S3Acl{
				RequestACL: fi.PtrTo("public-read"),
			}
		default:
			return nil, fmt.Errorf("the %q path does not support public ACL", p.Path())
		}
		return acl, nil
	}

	return acls.GetACL(ctx, p, c.T.Cluster)
}

func (_ *ManagedFile) Render(c *fi.CloudupContext, a, e, changes *ManagedFile) error {
	ctx := c.Context()

	location := fi.ValueOf(e.Location)
	if location == "" {
		return fi.RequiredField("Location")
	}

	data, err := fi.ResourceAsBytes(e.Contents)
	if err != nil {
		return fmt.Errorf("error reading contents of ManagedFile: %v", err)
	}

	p, err := getBasePath(c, e)
	if err != nil {
		return err
	}
	p = p.Join(location)

	acl, err := e.getACL(c, p)
	if err != nil {
		return err
	}

	err = p.WriteFile(ctx, bytes.NewReader(data), acl)
	if err != nil {
		return fmt.Errorf("error creating ManagedFile %q: %v", location, err)
	}

	return nil
}

func getBasePath(c *fi.CloudupContext, e *ManagedFile) (vfs.Path, error) {
	base := fi.ValueOf(e.Base)
	if base != "" {
		p, err := vfs.Context.BuildVfsPath(base)
		if err != nil {
			return nil, fmt.Errorf("error parsing ManagedFile Base %q: %v", base, err)
		}
		return p, nil
	}

	return c.T.ClusterConfigBase, nil
}

// RenderTerraform is responsible for rendering the terraform json.
func (f *ManagedFile) RenderTerraform(c *fi.CloudupContext, t *terraform.TerraformTarget, a, e, changes *ManagedFile) error {
	if !featureflag.TerraformManagedFiles.Enabled() {
		return f.Render(c, a, e, changes)
	}

	location := fi.ValueOf(e.Location)
	if location == "" {
		return fi.RequiredField("Location")
	}

	p, err := getBasePath(c, e)
	if err != nil {
		return err
	}
	p = p.Join(location)

	acl, err := e.getACL(c, p)
	if err != nil {
		return err
	}

	terraformPath, ok := p.(vfs.TerraformPath)
	if !ok {
		return fmt.Errorf("path %q must be of a type that can render in Terraform", p)
	}

	reader, err := e.Contents.Open()
	if err != nil {
		return err
	}

	return terraformPath.RenderTerraform(&t.TerraformWriter, *e.Name, reader, acl)
}

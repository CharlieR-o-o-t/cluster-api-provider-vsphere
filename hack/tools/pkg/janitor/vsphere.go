/*
Copyright 2024 The Kubernetes Authors.

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

package janitor

import (
	"context"
	"net/url"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/cns"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/list"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	ctrl "sigs.k8s.io/controller-runtime"
)

// NewVSphereClientsInput defines inputs for NewVSphereClients.
type NewVSphereClientsInput struct {
	Password   string
	Server     string
	Thumbprint string
	UserAgent  string
	Username   string
}

// VSphereClients is a collection of different clients for vSphere.
type VSphereClients struct {
	Vim           *vim25.Client
	Govmomi       *govmomi.Client
	Rest          *rest.Client
	FieldsManager *object.CustomFieldsManager
	Finder        *find.Finder
	ViewManager   *view.Manager
	CNS           *cns.Client
}

// Logout logs out all clients. It logs errors if the context contains a logger.
func (v *VSphereClients) Logout(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx)
	if err := v.Govmomi.Logout(ctx); err != nil {
		log.Error(err, "logging out govmomi client")
	}

	if err := v.Rest.Logout(ctx); err != nil {
		log.Error(err, "logging out rest client")
	}
}

// NewVSphereClients creates a VSphereClients object from the given input.
func NewVSphereClients(ctx context.Context, input NewVSphereClientsInput) (*VSphereClients, error) {
	urlCredentials := url.UserPassword(input.Username, input.Password)

	serverURL, err := soap.ParseURL(input.Server)
	if err != nil {
		return nil, err
	}
	serverURL.User = urlCredentials
	var soapClient *soap.Client
	if input.Thumbprint == "" {
		soapClient = soap.NewClient(serverURL, true)
	} else {
		soapClient = soap.NewClient(serverURL, false)
		soapClient.SetThumbprint(serverURL.Host, input.Thumbprint)
	}
	soapClient.UserAgent = input.UserAgent

	vimClient, err := vim25.NewClient(ctx, soapClient)
	if err != nil {
		return nil, err
	}

	govmomiClient := &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	if err := govmomiClient.Login(ctx, urlCredentials); err != nil {
		return nil, err
	}

	restClient := rest.NewClient(vimClient)
	if err := restClient.Login(ctx, urlCredentials); err != nil {
		return nil, err
	}

	fieldsManager, err := object.GetCustomFieldsManager(vimClient)
	if err != nil {
		return nil, err
	}

	viewManager := view.NewManager(vimClient)
	finder := find.NewFinder(vimClient, false)
	dc, err := finder.Datacenter(ctx, "*")
	if err != nil {
		return nil, err
	}
	finder.SetDatacenter(dc)

	cnsClient, err := cns.NewClient(ctx, vimClient)
	if err != nil {
		return nil, err
	}

	return &VSphereClients{
		Vim:           vimClient,
		Govmomi:       govmomiClient,
		Rest:          restClient,
		FieldsManager: fieldsManager,
		Finder:        finder,
		ViewManager:   viewManager,
		CNS:           cnsClient,
	}, nil
}

func waitForTasksFinished(ctx context.Context, tasks []*object.Task, ignoreErrors bool) error {
	for _, t := range tasks {
		if err := t.Wait(ctx); !ignoreErrors && err != nil {
			return err
		}
	}
	return nil
}

type managedElement struct {
	entity  mo.ManagedEntity
	element *list.Element
}

func recursiveList(ctx context.Context, inventoryPath string, govmomiClient *govmomi.Client, finder *find.Finder, viewManager *view.Manager, objectTypes ...string) ([]*managedElement, error) {
	// Get the object at inventoryPath
	objList, err := finder.ManagedObjectList(ctx, inventoryPath)
	if err != nil {
		return nil, err
	}
	if len(objList) != 1 {
		return nil, errors.Errorf("expected to find exactly 1 object at managed object at path: %s", inventoryPath)
	}

	root := objList[0].Object.Reference()

	v, err := viewManager.CreateContainerView(ctx, root, objectTypes, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = v.Destroy(ctx) }()

	// Recursively find all objects.
	managedObjects, err := v.Find(ctx, nil, property.Match{"name": "*"})
	if err != nil {
		return nil, err
	}

	managedElements := []*managedElement{}

	if len(managedObjects) == 0 {
		return managedElements, nil
	}

	// Retrieve the availableField and value attributes of the found object so we
	// later can check for the deletion marker.
	var objs []mo.ManagedEntity
	if err := govmomiClient.Retrieve(ctx, managedObjects, []string{"availableField", "value"}, &objs); err != nil {
		return nil, err
	}

	for _, entity := range objs {
		element, err := finder.Element(ctx, entity.Reference())
		if err != nil {
			return nil, err
		}
		managedElements = append(managedElements, &managedElement{entity: entity, element: element})
	}

	return managedElements, nil
}

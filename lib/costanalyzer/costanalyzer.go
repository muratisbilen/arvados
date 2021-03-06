// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package costanalyzer

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"git.arvados.org/arvados.git/lib/config"
	"git.arvados.org/arvados.git/sdk/go/arvados"
	"git.arvados.org/arvados.git/sdk/go/arvadosclient"
	"git.arvados.org/arvados.git/sdk/go/keepclient"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type nodeInfo struct {
	// Legacy (records created by Arvados Node Manager with Arvados <= 1.4.3)
	Properties struct {
		CloudNode struct {
			Price float64
			Size  string
		} `json:"cloud_node"`
	}
	// Modern
	ProviderType string
	Price        float64
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return ""
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func parseFlags(prog string, args []string, loader *config.Loader, logger *logrus.Logger, stderr io.Writer) (exitCode int, uuids arrayFlags, resultsDir string, cache bool, err error) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), `
Usage:
  %s [options ...]

	This program analyzes the cost of Arvados container requests. For each uuid
	supplied, it creates a CSV report that lists all the containers used to
	fulfill the container request, together with the machine type and cost of
	each container.

	When supplied with the uuid of a container request, it will calculate the
	cost of that container request and all its children. When suplied with a
	project uuid or when supplied with multiple container request uuids, it will
	create a CSV report for each supplied uuid, as well as a CSV file with
	aggregate cost accounting for all supplied uuids. The aggregate cost report
	takes container reuse into account: if a container was reused between several
	container requests, its cost will only be counted once.

	To get the node costs, the progam queries the Arvados API for current cost
	data for each node type used. This means that the reported cost always
	reflects the cost data as currently defined in the Arvados API configuration
	file.

	Caveats:
	- the Arvados API configuration cost data may be out of sync with the cloud
	provider.
	- when generating reports for older container requests, the cost data in the
	Arvados API configuration file may have changed since the container request
	was fulfilled. This program uses the cost data stored at the time of the
	execution of the container, stored in the 'node.json' file in its log
	collection.

	In order to get the data for the uuids supplied, the ARVADOS_API_HOST and
	ARVADOS_API_TOKEN environment variables must be set.

Options:
`, prog)
		flags.PrintDefaults()
	}
	loglevel := flags.String("log-level", "info", "logging `level` (debug, info, ...)")
	flags.StringVar(&resultsDir, "output", "", "output `directory` for the CSV reports (required)")
	flags.Var(&uuids, "uuid", "Toplevel `project or container request` uuid. May be specified more than once. (required)")
	flags.BoolVar(&cache, "cache", true, "create and use a local disk cache of Arvados objects")
	err = flags.Parse(args)
	if err == flag.ErrHelp {
		err = nil
		exitCode = 1
		return
	} else if err != nil {
		exitCode = 2
		return
	}

	if len(uuids) < 1 {
		flags.Usage()
		err = fmt.Errorf("Error: no uuid(s) provided")
		exitCode = 2
		return
	}

	if resultsDir == "" {
		flags.Usage()
		err = fmt.Errorf("Error: output directory must be specified")
		exitCode = 2
		return
	}

	lvl, err := logrus.ParseLevel(*loglevel)
	if err != nil {
		exitCode = 2
		return
	}
	logger.SetLevel(lvl)
	if !cache {
		logger.Debug("Caching disabled\n")
	}
	return
}

func ensureDirectory(logger *logrus.Logger, dir string) (err error) {
	statData, err := os.Stat(dir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(dir, 0700)
		if err != nil {
			return fmt.Errorf("error creating directory %s: %s", dir, err.Error())
		}
	} else {
		if !statData.IsDir() {
			return fmt.Errorf("the path %s is not a directory", dir)
		}
	}
	return
}

func addContainerLine(logger *logrus.Logger, node nodeInfo, cr arvados.ContainerRequest, container arvados.Container) (csv string, cost float64) {
	csv = cr.UUID + ","
	csv += cr.Name + ","
	csv += container.UUID + ","
	csv += string(container.State) + ","
	if container.StartedAt != nil {
		csv += container.StartedAt.String() + ","
	} else {
		csv += ","
	}

	var delta time.Duration
	if container.FinishedAt != nil {
		csv += container.FinishedAt.String() + ","
		delta = container.FinishedAt.Sub(*container.StartedAt)
		csv += strconv.FormatFloat(delta.Seconds(), 'f', 0, 64) + ","
	} else {
		csv += ",,"
	}
	var price float64
	var size string
	if node.Properties.CloudNode.Price != 0 {
		price = node.Properties.CloudNode.Price
		size = node.Properties.CloudNode.Size
	} else {
		price = node.Price
		size = node.ProviderType
	}
	cost = delta.Seconds() / 3600 * price
	csv += size + "," + strconv.FormatFloat(price, 'f', 8, 64) + "," + strconv.FormatFloat(cost, 'f', 8, 64) + "\n"
	return
}

func loadCachedObject(logger *logrus.Logger, file string, uuid string, object interface{}) (reload bool) {
	reload = true
	if strings.Contains(uuid, "-j7d0g-") {
		// We do not cache projects, they have no final state
		return
	}
	// See if we have a cached copy of this object
	_, err := os.Stat(file)
	if err != nil {
		return
	}
	data, err := ioutil.ReadFile(file)
	if err != nil {
		logger.Errorf("error reading %q: %s", file, err)
		return
	}
	err = json.Unmarshal(data, &object)
	if err != nil {
		logger.Errorf("failed to unmarshal json: %s: %s", data, err)
		return
	}

	// See if it is in a final state, if that makes sense
	switch v := object.(type) {
	case *arvados.ContainerRequest:
		if v.State == arvados.ContainerRequestStateFinal {
			reload = false
			logger.Debugf("Loaded object %s from local cache (%s)\n", uuid, file)
		}
	case *arvados.Container:
		if v.State == arvados.ContainerStateComplete || v.State == arvados.ContainerStateCancelled {
			reload = false
			logger.Debugf("Loaded object %s from local cache (%s)\n", uuid, file)
		}
	}
	return
}

// Load an Arvados object.
func loadObject(logger *logrus.Logger, ac *arvados.Client, path string, uuid string, cache bool, object interface{}) (err error) {
	file := uuid + ".json"

	var reload bool
	var cacheDir string

	if !cache {
		reload = true
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			reload = true
			logger.Info("Unable to determine current user home directory, not using cache")
		} else {
			cacheDir = homeDir + "/.cache/arvados/costanalyzer/"
			err = ensureDirectory(logger, cacheDir)
			if err != nil {
				reload = true
				logger.Infof("Unable to create cache directory at %s, not using cache: %s", cacheDir, err.Error())
			} else {
				reload = loadCachedObject(logger, cacheDir+file, uuid, object)
			}
		}
	}
	if !reload {
		return
	}

	if strings.Contains(uuid, "-j7d0g-") {
		err = ac.RequestAndDecode(&object, "GET", "arvados/v1/groups/"+uuid, nil, nil)
	} else if strings.Contains(uuid, "-xvhdp-") {
		err = ac.RequestAndDecode(&object, "GET", "arvados/v1/container_requests/"+uuid, nil, nil)
	} else if strings.Contains(uuid, "-dz642-") {
		err = ac.RequestAndDecode(&object, "GET", "arvados/v1/containers/"+uuid, nil, nil)
	} else {
		err = fmt.Errorf("unsupported object type with UUID %q:\n  %s", uuid, err)
		return
	}
	if err != nil {
		err = fmt.Errorf("error loading object with UUID %q:\n  %s", uuid, err)
		return
	}
	encoded, err := json.MarshalIndent(object, "", " ")
	if err != nil {
		err = fmt.Errorf("error marshaling object with UUID %q:\n  %s", uuid, err)
		return
	}
	if cacheDir != "" {
		err = ioutil.WriteFile(cacheDir+file, encoded, 0644)
		if err != nil {
			err = fmt.Errorf("error writing file %s:\n  %s", file, err)
			return
		}
	}
	return
}

func getNode(arv *arvadosclient.ArvadosClient, ac *arvados.Client, kc *keepclient.KeepClient, cr arvados.ContainerRequest) (node nodeInfo, err error) {
	if cr.LogUUID == "" {
		err = errors.New("No log collection")
		return
	}

	var collection arvados.Collection
	err = ac.RequestAndDecode(&collection, "GET", "arvados/v1/collections/"+cr.LogUUID, nil, nil)
	if err != nil {
		err = fmt.Errorf("error getting collection: %s", err)
		return
	}

	var fs arvados.CollectionFileSystem
	fs, err = collection.FileSystem(ac, kc)
	if err != nil {
		err = fmt.Errorf("error opening collection as filesystem: %s", err)
		return
	}
	var f http.File
	f, err = fs.Open("node.json")
	if err != nil {
		err = fmt.Errorf("error opening file 'node.json' in collection %s: %s", cr.LogUUID, err)
		return
	}

	err = json.NewDecoder(f).Decode(&node)
	if err != nil {
		err = fmt.Errorf("error reading file 'node.json' in collection %s: %s", cr.LogUUID, err)
		return
	}
	return
}

func handleProject(logger *logrus.Logger, uuid string, arv *arvadosclient.ArvadosClient, ac *arvados.Client, kc *keepclient.KeepClient, resultsDir string, cache bool) (cost map[string]float64, err error) {

	cost = make(map[string]float64)

	var project arvados.Group
	err = loadObject(logger, ac, uuid, uuid, cache, &project)
	if err != nil {
		return nil, fmt.Errorf("error loading object %s: %s", uuid, err.Error())
	}

	var childCrs map[string]interface{}
	filterset := []arvados.Filter{
		{
			Attr:     "owner_uuid",
			Operator: "=",
			Operand:  project.UUID,
		},
		{
			Attr:     "requesting_container_uuid",
			Operator: "=",
			Operand:  nil,
		},
	}
	err = ac.RequestAndDecode(&childCrs, "GET", "arvados/v1/container_requests", nil, map[string]interface{}{
		"filters": filterset,
		"limit":   10000,
	})
	if err != nil {
		return nil, fmt.Errorf("error querying container_requests: %s", err.Error())
	}
	if value, ok := childCrs["items"]; ok {
		logger.Infof("Collecting top level container requests in project %s\n", uuid)
		items := value.([]interface{})
		for _, item := range items {
			itemMap := item.(map[string]interface{})
			crCsv, err := generateCrCsv(logger, itemMap["uuid"].(string), arv, ac, kc, resultsDir, cache)
			if err != nil {
				return nil, fmt.Errorf("error generating container_request CSV: %s", err.Error())
			}
			for k, v := range crCsv {
				cost[k] = v
			}
		}
	} else {
		logger.Infof("No top level container requests found in project %s\n", uuid)
	}
	return
}

func generateCrCsv(logger *logrus.Logger, uuid string, arv *arvadosclient.ArvadosClient, ac *arvados.Client, kc *keepclient.KeepClient, resultsDir string, cache bool) (cost map[string]float64, err error) {

	cost = make(map[string]float64)

	csv := "CR UUID,CR name,Container UUID,State,Started At,Finished At,Duration in seconds,Compute node type,Hourly node cost,Total cost\n"
	var tmpCsv string
	var tmpTotalCost float64
	var totalCost float64

	// This is a container request, find the container
	var cr arvados.ContainerRequest
	err = loadObject(logger, ac, uuid, uuid, cache, &cr)
	if err != nil {
		return nil, fmt.Errorf("error loading cr object %s: %s", uuid, err)
	}
	var container arvados.Container
	err = loadObject(logger, ac, uuid, cr.ContainerUUID, cache, &container)
	if err != nil {
		return nil, fmt.Errorf("error loading container object %s: %s", cr.ContainerUUID, err)
	}

	topNode, err := getNode(arv, ac, kc, cr)
	if err != nil {
		return nil, fmt.Errorf("error getting node %s: %s", cr.UUID, err)
	}
	tmpCsv, totalCost = addContainerLine(logger, topNode, cr, container)
	csv += tmpCsv
	totalCost += tmpTotalCost
	cost[container.UUID] = totalCost

	// Find all container requests that have the container we found above as requesting_container_uuid
	var childCrs arvados.ContainerRequestList
	filterset := []arvados.Filter{
		{
			Attr:     "requesting_container_uuid",
			Operator: "=",
			Operand:  container.UUID,
		}}
	err = ac.RequestAndDecode(&childCrs, "GET", "arvados/v1/container_requests", nil, map[string]interface{}{
		"filters": filterset,
		"limit":   10000,
	})
	if err != nil {
		return nil, fmt.Errorf("error querying container_requests: %s", err.Error())
	}
	logger.Infof("Collecting child containers for container request %s", uuid)
	for _, cr2 := range childCrs.Items {
		logger.Info(".")
		node, err := getNode(arv, ac, kc, cr2)
		if err != nil {
			return nil, fmt.Errorf("error getting node %s: %s", cr2.UUID, err)
		}
		logger.Debug("\nChild container: " + cr2.ContainerUUID + "\n")
		var c2 arvados.Container
		err = loadObject(logger, ac, uuid, cr2.ContainerUUID, cache, &c2)
		if err != nil {
			return nil, fmt.Errorf("error loading object %s: %s", cr2.ContainerUUID, err)
		}
		tmpCsv, tmpTotalCost = addContainerLine(logger, node, cr2, c2)
		cost[cr2.ContainerUUID] = tmpTotalCost
		csv += tmpCsv
		totalCost += tmpTotalCost
	}
	logger.Info(" done\n")

	csv += "TOTAL,,,,,,,,," + strconv.FormatFloat(totalCost, 'f', 8, 64) + "\n"

	// Write the resulting CSV file
	fName := resultsDir + "/" + uuid + ".csv"
	err = ioutil.WriteFile(fName, []byte(csv), 0644)
	if err != nil {
		return nil, fmt.Errorf("error writing file with path %s: %s", fName, err.Error())
	}
	logger.Infof("\nUUID report in %s\n\n", fName)

	return
}

func costanalyzer(prog string, args []string, loader *config.Loader, logger *logrus.Logger, stdout, stderr io.Writer) (exitcode int, err error) {
	exitcode, uuids, resultsDir, cache, err := parseFlags(prog, args, loader, logger, stderr)
	if exitcode != 0 {
		return
	}
	err = ensureDirectory(logger, resultsDir)
	if err != nil {
		exitcode = 3
		return
	}

	// Arvados Client setup
	arv, err := arvadosclient.MakeArvadosClient()
	if err != nil {
		err = fmt.Errorf("error creating Arvados object: %s", err)
		exitcode = 1
		return
	}
	kc, err := keepclient.MakeKeepClient(arv)
	if err != nil {
		err = fmt.Errorf("error creating Keep object: %s", err)
		exitcode = 1
		return
	}

	ac := arvados.NewClientFromEnv()

	cost := make(map[string]float64)
	for _, uuid := range uuids {
		if strings.Contains(uuid, "-j7d0g-") {
			// This is a project (group)
			cost, err = handleProject(logger, uuid, arv, ac, kc, resultsDir, cache)
			if err != nil {
				exitcode = 1
				return
			}
			for k, v := range cost {
				cost[k] = v
			}
		} else if strings.Contains(uuid, "-xvhdp-") {
			// This is a container request
			var crCsv map[string]float64
			crCsv, err = generateCrCsv(logger, uuid, arv, ac, kc, resultsDir, cache)
			if err != nil {
				err = fmt.Errorf("Error generating container_request CSV for uuid %s: %s", uuid, err.Error())
				exitcode = 2
				return
			}
			for k, v := range crCsv {
				cost[k] = v
			}
		} else if strings.Contains(uuid, "-tpzed-") {
			// This is a user. The "Home" project for a user is not a real project.
			// It is identified by the user uuid. As such, cost analysis for the
			// "Home" project is not supported by this program. Skip this uuid, but
			// keep going.
			logger.Errorf("Cost analysis is not supported for the 'Home' project: %s", uuid)
		}
	}

	if len(cost) == 0 {
		logger.Info("Nothing to do!\n")
		return
	}

	var csv string

	csv = "# Aggregate cost accounting for uuids:\n"
	for _, uuid := range uuids {
		csv += "# " + uuid + "\n"
	}

	var total float64
	for k, v := range cost {
		csv += k + "," + strconv.FormatFloat(v, 'f', 8, 64) + "\n"
		total += v
	}

	csv += "TOTAL," + strconv.FormatFloat(total, 'f', 8, 64) + "\n"

	// Write the resulting CSV file
	aFile := resultsDir + "/" + time.Now().Format("2006-01-02-15-04-05") + "-aggregate-costaccounting.csv"
	err = ioutil.WriteFile(aFile, []byte(csv), 0644)
	if err != nil {
		err = fmt.Errorf("Error writing file with path %s: %s", aFile, err.Error())
		exitcode = 1
		return
	}
	logger.Infof("Aggregate cost accounting for all supplied uuids in %s\n", aFile)
	return
}

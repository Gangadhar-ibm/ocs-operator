package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"

	deploymanager "github.com/red-hat-storage/ocs-operator/pkg/deploy-manager"
)

var (
	ocsRegistryImage       = flag.String("ocs-registry-image", "", "The ocs-registry container image to use in the deployment")
	ocsSubscriptionChannel = flag.String("ocs-subscription-channel", "", "The subscription channel to receive upgades from in the deployment")
	yamlOutputPath         = flag.String("yaml-output-path", "", "Just generate the yaml for the OCS olm deployment and dump it to a file")
	arbiterEnabled         = flag.Bool("arbiter", false, "Deploy the StorageCluster with arbiter enabled")
)

func main() {

	flag.Parse()
	if *ocsRegistryImage == "" {
		log.Fatal("--ocs-registry-image is required")
	} else if *ocsSubscriptionChannel == "" {
		log.Fatal("--ocs-subscription-channel is required")
	}

	t, _ := deploymanager.NewDeployManager()

	if *arbiterEnabled {
		t.EnableArbiter()
	}

	if *yamlOutputPath != "" {
		yaml := t.DumpYAML(*ocsRegistryImage, *ocsSubscriptionChannel)
		err := ioutil.WriteFile(*yamlOutputPath, []byte(yaml), 0644)
		if err != nil {
			panic(err)
		}
		os.Exit(0)
	}

	log.Printf("Deploying ocs image %s", *ocsRegistryImage)
	err := t.DeployOCSWithOLM(*ocsRegistryImage, *ocsSubscriptionChannel)
	if err != nil {
		panic(err)
	}

	log.Printf("Starting default storage cluster")
	err = t.StartDefaultStorageCluster()
	if err != nil {
		panic(err)
	}

	log.Printf("Deployment successful")
}

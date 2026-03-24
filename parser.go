package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Image string `yaml:"image"`
	Steps []Step `yaml:"steps"`
}

type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

func main() {
	data, err := os.ReadFile(".jennings/workflows/build.yaml")
	if err != nil {
		panic(err)
	}

	var wf Workflow
	err = yaml.Unmarshal(data, &wf)
	if err != nil {
		panic(err)
	}

	// Access extracted values
	fmt.Println("Image:", wf.Image)

	fmt.Println("\nSteps:")
	for _, step := range wf.Steps {
		fmt.Printf("- %s: %s\n", step.Name, step.Run)
	}
}

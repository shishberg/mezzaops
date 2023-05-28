package task

import (
	"log"
	"os"
)

type Tasks struct {
	Tasks []*Task `yaml:"task"`
}

func ReadConfig(filename string) (*Tasks, error) {
	yaml, err := os.ReadFile(filename)
	if err != nil {
		log.Fatal(err)
	}
	tasks, err := ParseYAML(yaml)
	if err != nil {
		log.Fatal(err)
	}
	return &tasks, nil
}

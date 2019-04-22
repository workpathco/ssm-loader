package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
)

var paramInterpolation = regexp.MustCompile("%%(.*)%%")

type getParametersInput struct {
	Client        *ssm.SSM
	Path          *string
	NextToken     *string
	FetchedParams []*ssm.Parameter
}

type paramMap map[string]string

func getParameters(params *getParametersInput, itr int) ([]*ssm.Parameter, error) {
	if itr != 0 && params.NextToken == nil {
		return params.FetchedParams, nil
	}

	// Sleep for a tenth of a second before doing the next fetch
	// so we don't get rate-limited
	time.Sleep(100 * time.Millisecond)

	result, err := params.Client.GetParametersByPath(&ssm.GetParametersByPathInput{
		Path:           params.Path,
		NextToken:      params.NextToken,
		Recursive:      aws.Bool(false),
		MaxResults:     aws.Int64(10),
		WithDecryption: aws.Bool(true),
	})

	if err != nil {
		return nil, err
	}

	return getParameters(&getParametersInput{
		Client:        params.Client,
		Path:          params.Path,
		NextToken:     result.NextToken,
		FetchedParams: append(params.FetchedParams, result.Parameters...),
	}, itr+1)
}

func getOSEnv() paramMap {
	m := make(paramMap)

	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		m[pair[0]] = pair[1]
	}

	return m
}

func (m paramMap) AddParams(params []*ssm.Parameter) {
	for _, param := range params {
		ss := strings.Split(*param.Name, "/")
		name := ss[len(ss)-1]
		_, exists := m[name]
		if !exists {
			m[name] = *param.Value
		}
	}
}

func (m paramMap) ReplaceInterpolations() {
	for key, value := range m {
		replaced := paramInterpolation.ReplaceAllStringFunc(value, func(s string) string {
			varName := strings.Trim(s, "%")
			replacement, exists := m[varName]

			if exists {
				return replacement
			}

			return ""
		})
		m[key] = replaced
	}
}

func (m paramMap) StringArray() []string {
	i := len(m)
	list := make([]string, i)
	n := 0

	for k, v := range m {
		list[n] = fmt.Sprintf("%s=%s", k, v)
		n = n + 1
	}

	return list
}

func (m paramMap) SetOSEnv() {
	for key, value := range m {
		os.Setenv(key, value)
	}
}

func main() {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	svc := ssm.New(sess)

	appName := os.Getenv("APP_NAME")
	appEnv := os.Getenv("APP_ENV")

	paramMap := getOSEnv()

	if appEnv == "" {
		appEnv = os.Getenv("WORKPATH_ENV")
	}

	var allParams []*ssm.Parameter

	if appEnv != "" {
		sharedParams, sharedErr := getParameters(&getParametersInput{
			Client: svc,
			Path:   aws.String(fmt.Sprintf("/%s/", appEnv)),
		}, 0)

		if sharedErr != nil {
			log.Fatalln("Error fetching shared params: ", sharedErr.Error())
		}

		allParams = sharedParams
	}

	if appName != "" {
		appParams, appErr := getParameters(&getParametersInput{
			Client: svc,
			Path:   aws.String(fmt.Sprintf("/%s/%s/", appEnv, appName)),
		}, 0)

		if appErr != nil {
			log.Fatalln("Error fetching app params: ", appErr.Error())
		}

		allParams = append(allParams, appParams...)
	}

	paramMap.AddParams(allParams)
	paramMap.ReplaceInterpolations()

	// Grab runner args
	args := os.Args[1:]

	// Set command to first arg

	if len(args) == 0 {
		log.Fatalln("ssm-loader requires a command to run")
	}

	cmd := exec.Command(args[0], args[1:]...)

	// Pipe everything to the command
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = paramMap.StringArray()

	err := cmd.Start()
	if err != nil {
		log.Fatalln("Error while starting command: ", err)
	}

	err = cmd.Wait()

	if err != nil {
		log.Fatalln("Command finished with err: ", err)
	}
}

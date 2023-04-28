package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rancher/wrangler/pkg/signals"
	"github.com/spf13/cobra"

	"kraftkit.sh/cmdfactory"
)

const PROTOBUF_HEADER = `syntax = "proto3";

package qmp.v1alpha;

import "machine/qemu/qmp/v1alpha/descriptor.proto";

option go_package = "kraftkit.sh/machine/qemu/qmp/v1alpha;qmpv1alpha";

`

const DESCRIPTOR = `syntax = "proto3";

package qmp.v1alpha;

import "google/protobuf/any.proto";
import "google/protobuf/descriptor.proto";

option go_package = "kraftkit.sh/machine/qemu/qmp/v1alpha;qmpv1alpha";

extend google.protobuf.MessageOptions {
	string execute = 51000;
}

extend google.protobuf.EnumValueOptions {
	string json_name   = 51001;
	string map_message = 51002;
}
`

type Qapi2Proto struct{}

func New() *cobra.Command {
	return cmdfactory.New(&Qapi2Proto{}, cobra.Command{
		Short: "Generate Protobuf files from QAPI schema",
		Long:  "Generate Protobuf files from QAPI schema",
		Args:  cobra.ExactArgs(2),
		Use:   "qapi2proto QEMUDir OutputDir",
	})
}

func (opts *Qapi2Proto) Run(cmd *cobra.Command, args []string) error {
	// Get the QAPI and output directories from arguments
	qapiDir := filepath.Join(args[0], "qapi")
	outDir := args[1]

	// Try to read the JSON files
	qapiFiles, err := os.ReadDir(qapiDir)
	if err != nil {
		return fmt.Errorf("Unable to read QAPI files in directory %s (%v)", qapiDir, err)
	}

	// Create the output directory if it doesn't exist
	err = os.MkdirAll(outDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("Unable to create output directory %s (%v)", outDir, err)
	}

	eventNames := []string{}
	commandNames := []string{}
	// Iterate through all files in QAPI directory, filtering for JSON files
	for _, file := range qapiFiles {
		if file.Type().IsRegular() && strings.HasSuffix(file.Name(), ".json") {
			// Open the file
			filePath := filepath.Join(qapiDir, file.Name())
			dataBytes, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}

			fileLines := strings.Split(string(dataBytes[:]), "\n")
			jsonLines, commentLines := []string{}, []string{}
			for _, line := range fileLines {
				strippedLine := strings.TrimSpace(line)
				if len(strippedLine) == 0 {
					continue
				} else if strings.HasPrefix(strippedLine, "#") {
					commentLines = append(commentLines, strippedLine)
				} else {
					jsonLines = append(jsonLines, strippedLine)
				}
			}

			allComments := ParseComments(commentLines)
			qapiDefs := ParseJSON(jsonLines)

			output := ""
			for _, qapiDef := range qapiDefs {
				structName, isStruct := qapiDef["struct"]
				enumName, isEnum := qapiDef["enum"]
				eventName, isEvent := qapiDef["event"]
				commandName, isCommand := qapiDef["command"]
				if isStruct {
					comments, ok := allComments[structName.(string)]["Info"]
					if ok {
						for _, comment := range comments {
							output += fmt.Sprintf("// %s\n", comment)
						}
					}

					output += fmt.Sprintf("message %s {\n", structName)
					values, _ := qapiDef["data"]
					for field, value := range values.(map[string]interface{}) {
						typeName := ""
						repeated := ""
						switch v := value.(type) {
						case(string):
							typeName = v
						case(map[string]interface{}):
							typeName, _ = v["type"].(string)
						case([]interface{}):
							repeated = "repeated "
							typeName = v[0].(string)
						}
						valueComments, ok := allComments[structName.(string)][field]
						if ok {
							for _, comment := range valueComments {
								output += fmt.Sprintf("\t// %s\n", comment)
							}
						}
						output += fmt.Sprintf("\t%s%s %s;\n", repeated, typeName, field)
					}
					output += "}\n\n"

				} else if isEnum {
					comments, ok := allComments[enumName.(string)]["Info"]
					if ok {
						for _, comment := range comments {
							output += fmt.Sprintf("// %s\n", comment)
						}
					}

					output += fmt.Sprintf("enum %s {\n", enumName)
					values, valid := qapiDef["data"]
					if valid {
						for idx, value := range values.([]interface{}) {
							valueName := ""
							switch v := value.(type) {
							case (string):
								valueName = v
							case (map[string]interface{}):
								valueName, _ = v["name"].(string)
							} 
							valueComments, ok := allComments[enumName.(string)][valueName]
							if ok {
								for _, comment := range valueComments {
									output += fmt.Sprintf("\t// %s\n", comment)
								}
							}
							output += fmt.Sprintf("\t%s = %d;\n", valueName, idx)
						}
					}
					output += "}\n\n"
				} else if isEvent {
					eventNames = append(eventNames, eventName.(string))
				} else if isCommand {
					commandNames = append(commandNames, commandName.(string))
				}
			}
			
			// Write output to the corresponding Protobuf file
			prefix, _, _ := strings.Cut(file.Name(), ".")
			f, _ := os.Create(filepath.Join(outDir, prefix + ".proto"))
			f.WriteString(PROTOBUF_HEADER)
			f.WriteString(output)
			f.Close()
		}
	}

	// Write the descriptor protobuf
	f, _ := os.Create(filepath.Join(outDir, "descriptor.proto"))
	f.WriteString(DESCRIPTOR)
	f.Close()

	// Write the service protobuf 
	f, _ = os.Create(filepath.Join(outDir, "service.proto"))
	f.WriteString(PROTOBUF_HEADER)
	f.WriteString(fmt.Sprintf("service QEMUMachineProtocol {\n"))
	for _, commandName := range commandNames {
		f.WriteString(fmt.Sprintf("\trpc %s(%sRequest) returns (%sResponse) {}\n", commandName, commandName, commandName))
	}
	f.WriteString("}\n")
	f.Close()

	// Sort the events to create the events protobuf
	sort.Slice(eventNames, func(i, j int) bool {
		return eventNames[i] < eventNames[j]
	})

	f, _ = os.Create(filepath.Join(outDir, "events.proto"))
	f.WriteString(PROTOBUF_HEADER)
	f.WriteString(fmt.Sprintf("enum EventType {\n"))
	for idx, eventName := range eventNames {
		f.WriteString(fmt.Sprintf("\tEVENT_%s = %d [ (json_name) = \"%s\" ];\n", eventName, idx, eventName))
	}
	f.WriteString("}\n")
	f.Close()

	return nil
}

func ParseComment(comment []string) (string, map[string][]string) {
	// Trim all of the comment lines, removing empty lines
	trimmedLines := []string{}
	for _, line := range comment {
		trimmedLine := strings.Trim(line, "# ")
		if len(trimmedLine) > 0 {
			trimmedLines = append(trimmedLines, trimmedLine)
		}
	}
	trimmedLines = append(trimmedLines, "DUMMY:")

	// Keep track of the "root" comment title for each entry
	root := ""
	entries := make(map[string][]string)

	// Parse each field using Regex
	fieldRegex := regexp.MustCompile("@?([\\w-_]+?): ?(.*)")
	curFieldName := ""
	curFieldInfo := []string{}
	for _, line := range trimmedLines {
		res := fieldRegex.FindStringSubmatch(line)
		if len(res) > 0 {
			// If a new field has been found, save the current field
			if curFieldName != "" {
				if root == "" {
					root = curFieldName
					if len(curFieldInfo) > 0 {
						entries["Info"] = curFieldInfo
					}
				} else {
					entries[curFieldName] = curFieldInfo
				}
			}

			// Start keeping track of the new field
			curFieldName = res[1]
			if res[2] != "" {
				curFieldInfo = []string{res[2]}
			} else {
				curFieldInfo = []string{}
			}
		} else {
			curFieldInfo = append(curFieldInfo, line)
		}
	}

	return root, entries
}

func ParseComments(commentLines []string) map[string]map[string][]string {
	comments := make(map[string]map[string][]string)
	startIdx := -1
	for idx, line := range commentLines {
		if line == "##" {
			if startIdx != -1 {
				currComment := commentLines[startIdx+1 : idx]
				if !strings.HasPrefix(currComment[0], "# =") {
					entry, fields := ParseComment(currComment)
					comments[entry] = fields
				}
				startIdx = -1
			} else {
				startIdx = idx
			}
		}
	}

	return comments
}

func ParseJSON(jsonLines []string) []map[string]interface{} {
	// QAPI is not exactly JSON
	// Need to switch to double-quotes and remove any rare inline-comments
	fixedJson := make([]string, len(jsonLines))
	for idx, line := range jsonLines {
		fixedLine := strings.ReplaceAll(line, "'", "\"")
		commentIdx := strings.LastIndexByte(fixedLine, '#')
		if commentIdx != -1 {
			fixedLine = fixedLine[:commentIdx]
		}

		fixedJson[idx] = fixedLine
	}

	var schemaDefs []map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(strings.Join(fixedJson, "")))
	for {
		var schemaDef map[string]interface{}
		err := dec.Decode(&schemaDef)
		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		schemaDefs = append(schemaDefs, schemaDef)
	}
	return schemaDefs
}

func main() {
	cmdfactory.Main(signals.SetupSignalContext(), New())
}

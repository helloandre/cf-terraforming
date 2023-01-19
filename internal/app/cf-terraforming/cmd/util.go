package cmd

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	cloudflare "github.com/cloudflare/cloudflare-go"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zclconf/go-cty/cty"
)

var hasNumber = regexp.MustCompile("[0-9]+").MatchString

func contains(slice []string, item string) bool {
	set := make(map[string]struct{}, len(slice))
	for _, s := range slice {
		set[s] = struct{}{}
	}

	_, ok := set[item]
	return ok
}

func executeCommandC(root *cobra.Command, args ...string) (output string, err error) {
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	_, err = root.ExecuteC()

	return buf.String(), err
}

// testDataFile slurps a local test case into memory and returns it while
// encapsulating the logic for finding it.
func testDataFile(filename string) string {
	filename = strings.TrimSuffix(filename, "/")

	dirname, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	dir, err := os.Open(filepath.Join(dirname, "../../../../testdata/terraform"))
	if err != nil {
		panic(err)
	}

	fullpath := dir.Name() + "/" + filename + "/test.tf"
	if _, err := os.Stat(fullpath); os.IsNotExist(err) {
		panic(fmt.Errorf("terraform testdata file does not exist at %s", fullpath))
	}

	data, _ := ioutil.ReadFile(fullpath)

	return string(data)
}

func sharedPreRun(cmd *cobra.Command, args []string) {
	accountID = viper.GetString("account")
	zoneID = viper.GetString("zone")
	hostname = viper.GetString("hostname")

	if accountID != "" && zoneID != "" {
		log.Fatal("--account and --zone are mutually exclusive, support for both is deprecated")
	}

	if apiToken = viper.GetString("token"); apiToken == "" {
		if apiEmail = viper.GetString("email"); apiEmail == "" {
			log.Error("'email' must be set.")
		}

		if apiKey = viper.GetString("key"); apiKey == "" {
			log.Error("either -t/--token or -k/--key must be set.")
		}

		log.WithFields(logrus.Fields{
			"email":      apiEmail,
			"zone_id":    zoneID,
			"account_id": accountID,
		}).Debug("initializing cloudflare-go")
	} else {
		log.WithFields(logrus.Fields{
			"zone_id":    zoneID,
			"account_Id": accountID,
		}).Debug("initializing cloudflare-go with API Token")
	}

	var options []cloudflare.Option

	if hostname != "" {
		options = append(options, cloudflare.BaseURL("https://"+hostname+"/client/v4"))
	}

	if verbose {
		options = append(options, cloudflare.Debug(true))
	}

	var err error

	// Don't initialise a client in CI as this messes with VCR and the ability to
	// mock out the HTTP interactions.
	if os.Getenv("CI") != "true" {
		var useToken = apiToken != ""

		if useToken {
			api, err = cloudflare.NewWithAPIToken(apiToken, options...)
		} else {
			api, err = cloudflare.New(apiKey, apiEmail, options...)
		}

		if err != nil {
			log.Fatal(err)
		}
	}
}

// sanitiseTerraformResourceName ensures that a Terraform resource name matches
// the restrictions imposed by core.
func sanitiseTerraformResourceName(s string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	return re.ReplaceAllString(s, "_")
}

// flattenAttrMap takes a list of attributes defined as a list of maps comprising of {"id": "attrId", "value": "attrValue"}
// and flattens it to a single map of {"attrId": "attrValue"}.
func flattenAttrMap(l []interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	attrID := ""
	var attrVal interface{}

	for _, elem := range l {
		switch t := elem.(type) {
		case map[string]interface{}:
			if id, ok := t["id"]; ok {
				attrID = id.(string)
			} else {
				log.Debug("no 'id' in map when attempting to flattenAttrMap")
			}

			if val, ok := t["value"]; ok {
				if val == nil {
					log.Debugf("Found nil 'value' for %s attempting to flattenAttrMap, coercing to true", attrID)
					attrVal = true
				} else {
					attrVal = val
				}
			} else {
				log.Debug("no 'value' in map when attempting to flattenAttrMap")
			}

			result[attrID] = attrVal
		default:
			log.Debugf("got unknown element type %T when attempting to flattenAttrMap", elem)
		}
	}

	return result
}

// nestBlocks takes a schema and generates all of the appropriate nesting of any
// top-level blocks as well as nested lists or sets.
func nestBlocks(schemaBlock *tfjson.SchemaBlock, structData map[string]interface{}, parentID string, indexedNestedBlocks map[string][]string) string {
	output := ""

	// Nested blocks are used for configuration options where assignment
	// isn't required.
	sortedNestedBlocks := make([]string, 0, len(schemaBlock.NestedBlocks))
	for k := range schemaBlock.NestedBlocks {
		sortedNestedBlocks = append(sortedNestedBlocks, k)
	}
	sort.Strings(sortedNestedBlocks)

	for _, block := range sortedNestedBlocks {
		if schemaBlock.NestedBlocks[block].NestingMode == "list" || schemaBlock.NestedBlocks[block].NestingMode == "set" {
			sortedInnerAttributes := make([]string, 0, len(schemaBlock.NestedBlocks[block].Block.Attributes))

			for k := range schemaBlock.NestedBlocks[block].Block.Attributes {
				sortedInnerAttributes = append(sortedInnerAttributes, k)
			}

			sort.Strings(sortedInnerAttributes)

			for attrName, attrConfig := range schemaBlock.NestedBlocks[block].Block.Attributes {
				if attrConfig.Computed && !attrConfig.Optional {
					schemaBlock.NestedBlocks[block].Block.Attributes[attrName].AttributeType = cty.NilType
				}
			}

			nestedBlockOutput := ""

			// If the attribute we're looking at has further nesting, we'll
			// recursively call nestBlocks.
			if len(schemaBlock.NestedBlocks[block].Block.NestedBlocks) > 0 {
				if s, ok := structData[block]; ok {
					switch s.(type) {
					case map[string]interface{}:
						nestedBlockOutput += nestBlocks(schemaBlock.NestedBlocks[block].Block, s.(map[string]interface{}), parentID, indexedNestedBlocks)
						indexedNestedBlocks[parentID] = append(indexedNestedBlocks[parentID], nestedBlockOutput)

					case []interface{}:
						var previousNextedBlockOutput string
						for _, nestedItem := range s.([]interface{}) {
							_, exists := nestedItem.(map[string]interface{})["id"]
							if exists {
								parentID = nestedItem.(map[string]interface{})["id"].(string)
							}
							if !exists {
								// if we fail to find an ID, we tag the current element with a uuid
								log.Debugf("id not found for nestedItem %#v using uuid terraform_internal_id", nestedItem)
								nestedItem.(map[string]interface{})["terraform_internal_id"] = parentID
							}

							nestedBlockOutput += nestBlocks(schemaBlock.NestedBlocks[block].Block, nestedItem.(map[string]interface{}), parentID, indexedNestedBlocks)

							if previousNextedBlockOutput != nestedBlockOutput {
								previousNextedBlockOutput = nestedBlockOutput
								// The indexedNestedBlocks maps helps us know which parent we're rendering the nested block for
								// So we append the current child's output to it, for when we render it out later
								indexedNestedBlocks[parentID] = append(indexedNestedBlocks[parentID], nestedBlockOutput)
							}
						}

					default:
						log.Debugf("unable to generate recursively nested blocks for %T", s)
					}
				}
			}

			switch attrStruct := structData[block].(type) {
			// Case for if the inner block's attributes are a map of interfaces, in
			// which case we can directly add them to the config.
			case map[string]interface{}:
				if attrStruct != nil {
					nestedBlockOutput += writeNestedBlock(sortedInnerAttributes, schemaBlock.NestedBlocks[block].Block, attrStruct)
				}

				if nestedBlockOutput != "" || schemaBlock.NestedBlocks[block].MinItems > 0 {
					output += block + " {\n"
					output += nestedBlockOutput
					output += "}\n"
				}

			// Case for if the inner block's attributes are a list of map interfaces,
			// in which case this should be treated as a duplicating block.
			case []map[string]interface{}:
				for _, v := range attrStruct {
					repeatedBlockOutput := ""

					if attrStruct != nil {
						repeatedBlockOutput = writeNestedBlock(sortedInnerAttributes, schemaBlock.NestedBlocks[block].Block, v)
					}

					// Write the block if we had data for it, or if it is a required block.
					if repeatedBlockOutput != "" || schemaBlock.NestedBlocks[block].MinItems > 0 {
						output += block + " {\n"
						output += repeatedBlockOutput

						if nestedBlockOutput != "" {
							output += nestedBlockOutput
						}

						output += "}\n"
					}
				}

			// Case for duplicated blocks that commonly end up as an array or list at
			// the API level.
			case []interface{}:
				for _, v := range attrStruct {
					repeatedBlockOutput := ""
					if attrStruct != nil {
						repeatedBlockOutput = writeNestedBlock(sortedInnerAttributes, schemaBlock.NestedBlocks[block].Block, v.(map[string]interface{}))
					}

					// Write the block if we had data for it, or if it is a required block.
					if repeatedBlockOutput != "" || schemaBlock.NestedBlocks[block].MinItems > 0 {
						output += block + " {\n"
						output += repeatedBlockOutput
						if nestedBlockOutput != "" {
							// We're processing the nested child blocks for currentId
							currentID, exists := v.(map[string]interface{})["id"].(string)
							if !exists {
								currentID = v.(map[string]interface{})["terraform_internal_id"].(string)
							}

							if len(indexedNestedBlocks[currentID]) > 0 {
								currentNestIdx := len(indexedNestedBlocks[currentID]) - 1
								// Pull out the last nestedblock that we built for this parent
								// We only need to render the last one because it holds every other all other nested blocks for this parent
								currentNest := indexedNestedBlocks[currentID][currentNestIdx]

								for ID, nest := range indexedNestedBlocks {
									if ID != currentID && len(nest) > 0 {
										// Itereate over all other indexed nested blocks and remove anything from the current block
										// that belongs to a different parent
										currentNest = strings.Replace(currentNest, nest[len(nest)-1], "", 1)
									}
								}
								// currentNest is all that needs to be rendered for this parent
								// re-index to make sure we capture the removal of the nested blocks that dont
								// belong to this parent
								indexedNestedBlocks[currentID][currentNestIdx] = currentNest
								output += currentNest
							}
						}

						output += "}\n"
					}
				}

			default:
				log.Debugf("unexpected attribute struct type %T for block %s", attrStruct, block)
			}
		} else {
			log.Debugf("nested mode %q for %s not recognised", schemaBlock.NestedBlocks[block].NestingMode, block)
		}
	}

	return output
}

func writeNestedBlock(attributes []string, schemaBlock *tfjson.SchemaBlock, attrStruct map[string]interface{}) string {
	nestedBlockOutput := ""

	for _, attrName := range attributes {
		ty := schemaBlock.Attributes[attrName].AttributeType

		switch {
		case ty.IsPrimitiveType():
			switch ty {
			case cty.String, cty.Bool, cty.Number:
				nestedBlockOutput += writeAttrLine(attrName, attrStruct[attrName], false)
			default:
				log.Debugf("unexpected primitive type %q", ty.FriendlyName())
			}
		case ty.IsListType(), ty.IsSetType():
			nestedBlockOutput += writeAttrLine(attrName, attrStruct[attrName], true)
		case ty.IsMapType():
			nestedBlockOutput += writeAttrLine(attrName, attrStruct[attrName], false)
		default:
			log.Debugf("unexpected nested type %T for %s", ty, attrName)
		}
	}

	return nestedBlockOutput
}

// writeAttrLine outputs a line of HCL configuration with a configurable depth
// for known types.
func writeAttrLine(key string, value interface{}, usedInBlock bool) string {
	switch values := value.(type) {
	case map[string]interface{}:
		sortedKeys := make([]string, 0, len(values))
		for k := range values {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)

		s := ""
		for _, v := range sortedKeys {
			// check if our key has an integer in the string. If it does we need to wrap it with quotes.
			if hasNumber(v) {
				s += writeAttrLine(fmt.Sprintf("\"%s\"", v), values[v], false)
			} else {
				s += writeAttrLine(v, values[v], false)
			}
		}

		if usedInBlock {
			if s != "" {
				return fmt.Sprintf("%s {\n%s}\n", key, s)
			}
		} else {
			if s != "" {
				return fmt.Sprintf("%s = {\n%s}\n", key, s)
			}
		}
	case []interface{}:
		var stringItems []string
		var intItems []int
		var interfaceItems []map[string]interface{}

		for _, item := range value.([]interface{}) {
			switch item := item.(type) {
			case string:
				stringItems = append(stringItems, item)
			case map[string]interface{}:
				interfaceItems = append(interfaceItems, item)
			case float64:
				intItems = append(intItems, int(item))
			}
		}
		if len(stringItems) > 0 {
			return writeAttrLine(key, stringItems, false)
		}

		if len(intItems) > 0 {
			return writeAttrLine(key, intItems, false)
		}

		if len(interfaceItems) > 0 {
			return writeAttrLine(key, interfaceItems, false)
		}

	case []map[string]interface{}:
		var stringyInterfaces []string
		var op string
		var mapLen = len(value.([]map[string]interface{}))
		for i, item := range value.([]map[string]interface{}) {
			// Use an empty key to prevent rendering the key
			op = writeAttrLine("", item, true)
			// if condition handles adding new line for just the last element
			if i != mapLen-1 {
				op = strings.TrimRight(op, "\n")
			}
			stringyInterfaces = append(stringyInterfaces, op)
		}
		return fmt.Sprintf("%s = [ \n%s ]\n", key, strings.Join(stringyInterfaces, ",\n"))

	case []int:
		stringyInts := []string{}
		for _, int := range value.([]int) {
			stringyInts = append(stringyInts, fmt.Sprintf("%d", int))
		}
		return fmt.Sprintf("%s = [ %s ]\n", key, strings.Join(stringyInts, ", "))
	case []string:
		var items []string
		for _, item := range value.([]string) {
			items = append(items, fmt.Sprintf("%q", item))
		}
		if len(items) > 0 {
			return fmt.Sprintf("%s = [ %s ]\n", key, strings.Join(items, ", "))
		}
	case string:
		if value != "" {
			return fmt.Sprintf("%s = %q\n", key, value)
		}
	case int:
		return fmt.Sprintf("%s = %d\n", key, value)
	case float64:
		return fmt.Sprintf("%s = %0.f\n", key, value)
	case bool:
		return fmt.Sprintf("%s = %t\n", key, value)
	default:
		log.Debugf("got unknown attribute configuration: key %s, value %v, value type %T", key, value, value)
		return ""
	}
	return ""
}

// Copyright 2016-2020 The grok_exporter Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tailer

import (
	"errors"
	"fmt"
	json "github.com/bitly/go-simplejson"
	configuration "github.com/fstab/grok_exporter/config/v3"
	"github.com/fstab/grok_exporter/tailer/fswatcher"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"strings"
)

type WebhookTailer struct {
	lines  chan *fswatcher.Line
	errors chan fswatcher.Error
	config *configuration.InputConfig
}

var webhookTailerSingleton *WebhookTailer

func (t *WebhookTailer) Lines() chan *fswatcher.Line {
	return t.lines
}

func (t *WebhookTailer) Errors() chan fswatcher.Error {
	return t.errors
}

func (t *WebhookTailer) Close() {
	// NO-OP, since the webserver thread is handled by the metrics server
}

func InitWebhookTailer(inputConfig *configuration.InputConfig) fswatcher.FileTailer {
	if webhookTailerSingleton != nil {
		return webhookTailerSingleton
	}

	lineChan := make(chan *fswatcher.Line)
	errorChan := make(chan fswatcher.Error)
	webhookTailerSingleton = &WebhookTailer{
		lines:  lineChan,
		errors: errorChan,
		config: inputConfig,
	}
	return webhookTailerSingleton
}

func WebhookHandler() http.Handler {
	return webhookTailerSingleton
}

func (t WebhookTailer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Implement the http handler interface

	wts := webhookTailerSingleton
	lineChan := wts.lines
	errorChan := wts.errors

	if r.Body == nil {
		err := errors.New("got empty request body")
		logrus.Warn(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		errorChan <- fswatcher.NewError(fswatcher.NotSpecified, err, "")
		return
	}

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		logrus.Warn(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		errorChan <- fswatcher.NewError(fswatcher.NotSpecified, err, "")
		return
	}
	defer r.Body.Close()

	lines := WebhookProcessBody(wts.config, b)
	for _, line := range lines {
		logrus.WithFields(logrus.Fields{
			"line": line,
		}).Debug("Groking line")
		lineChan <- &fswatcher.Line{Line: line}
	}
	return
}

func WebhookProcessBody(c *configuration.InputConfig, b []byte) []string {

	strs := []string{}

	switch c.WebhookFormat {
	case "text_single":
		s := strings.TrimSpace(string(b))
		strs = append(strs, s)
	case "text_bulk":
		s := strings.TrimSpace(string(b))
		strs = strings.Split(s, c.WebhookTextBulkSeparator)
	case "json_single":
		if len(c.WebhookJsonSelector) == 0 || c.WebhookJsonSelector[0] != '.' {
			logrus.Errorf("%v: invalid webhook json selector", c.WebhookJsonSelector)
			break
		}
		j, err := json.NewJson(b)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"post_body": string(b),
			}).Warn("Unable to Parse JSON")
			break
		}
		s, err := processPath(j, c.WebhookJsonSelector)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"post_body":             string(b),
				"webhook_json_selector": c.WebhookJsonSelector,
			}).Warn("Unable to find selector path")
			break
		}
		strs = append(strs, s)
	case "json_bulk":
		if len(c.WebhookJsonSelector) == 0 || c.WebhookJsonSelector[0] != '.' {
			logrus.Errorf("%v: invalid webhook json selector", c.WebhookJsonSelector)
			break
		}
		j, err := json.NewJson(b)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"post_body": string(b),
			}).Warn("Unable to Parse JSON")
			break
		}

		for _, ei := range j.MustArray() {
			// Cast the entry interface{} back to the Json object.
			//   Unfortunately, this is how the simplejson lib works.
			ej := json.New()
			ej.Set("x", ei)
			newSelector := fmt.Sprintf(".x.%v", c.WebhookJsonSelector[1:])
			s, err := processPath(ej, newSelector)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"post_body":             string(b),
					"webhook_json_selector": c.WebhookJsonSelector,
				}).Warn("Unable to find selector path")
				break
			}
			strs = append(strs, s)
		}
	default:
		// error silently
	}

	// Trim whitespace before and after every log entry
	for i := range strs {
		strs[i] = strings.TrimSpace(strs[i])
	}

	return strs
}

func processPath(json *json.Json, path string) (string, error) {
	if len(path) <= 1 {
		return "", fmt.Errorf("%q: invalid webhook json selector", path)
	}
	for _, pathElement := range strings.Split(path[1:], ".") {
		i := len(pathElement) - 1
		if i > 3 && pathElement[i] == ']' {
			name, index, err := parseJsonPathElement(pathElement)
			if err != nil {
				return "", fmt.Errorf("%q: invalid webhook json selector: %v", path, err)
			}
			json = json.GetPath(name)
			json = json.GetIndex(index)
		} else {
			json = json.GetPath(pathElement)
		}
	}
	return json.String()
}

// pathElement is a string like "messages[0]", this method splits it into "messages" and 0.
// We assume that pathElement ends with ']'.
func parseJsonPathElement(pathElement string) (string, int, error) {
	index := 0
	i := len(pathElement) - 2
	for ; i > 0 && pathElement[i] != '['; i-- {
		digit := pathElement[i] - '0'
		if digit < 0 || digit > 9 {
			return "", 0, fmt.Errorf("%q: path element ends with ']' but array index is invalid", pathElement)
		}
		index *= 10
		index += int(digit)
	}
	return pathElement[0:i], index, nil
}

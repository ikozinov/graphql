// Package graphql provides a low level GraphQL client.
//
//  // create a client (safe to share across requests)
//  client := graphql.NewClient("https://machinebox.io/graphql")
//
//  // make a request
//  req := graphql.NewRequest(`
//      query ($key: String!) {
//          items (id:$key) {
//              field1
//              field2
//              field3
//          }
//      }
//  `)
//
//  // set any variables
//  req.Var("key", "value")
//
//  // run it and capture the response
//  var respData ResponseStruct
//  if err := client.Run(ctx, req, &respData); err != nil {
//      log.Fatal(err)
//  }
//
// Specify client
//
// To specify your own http.Client, use the WithHTTPClient option:
//  httpclient := &http.Client{}
//  client := graphql.NewClient("https://machinebox.io/graphql", graphql.WithHTTPClient(httpclient))
package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// Client is a client for interacting with a GraphQL API.
type Client struct {
	endpoint         string
	httpClient       *http.Client
	useMultipartForm bool

	useMultipartRequestSpec bool

	// closeReq will close the request body immediately allowing for reuse of client
	closeReq bool

	// Log is called with various debug information.
	// To log to standard out, use:
	//  client.Log = func(s string) { log.Println(s) }
	Log func(s string)
}

// NewClient makes a new Client capable of making GraphQL requests.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: endpoint,
		Log:      func(string) {},
	}
	for _, optionFunc := range opts {
		optionFunc(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	return c
}

func (c *Client) logf(format string, args ...interface{}) {
	c.Log(fmt.Sprintf(format, args...))
}

// Run executes the query and unmarshals the response from the data field
// into the response object.
// Pass in a nil response object to skip response parsing.
func (c *Client) Run(ctx context.Context, req *Request, resp interface{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(req.files) > 0 && !(c.useMultipartForm || c.useMultipartRequestSpec) {
		return errors.New("cannot send files with PostFields option")
	}
	if c.useMultipartForm {
		return c.runWithPostFields(ctx, req, resp)
	}
	if c.useMultipartRequestSpec && len(req.Files()) > 0 {
		return c.runMultipartRequestSpec(ctx, req, resp)
	}
	return c.runWithJSON(ctx, req, resp)
}

func (c *Client) runWithJSON(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}
	if err := json.NewEncoder(&requestBody).Encode(requestBodyObj); err != nil {
		return errors.Wrap(err, "encode body")
	}
	c.logf(">> variables: %v", req.vars)
	c.logf(">> query: %s", req.q)
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &requestBody)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	c.logf(">> headers: %v", r.Header)
	r = r.WithContext(ctx)
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return errors.Wrap(err, "reading body")
	}
	c.logf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return errors.Wrap(err, "decoding response")
	}
	if len(gr.Errors) > 0 {
		return gr.Errors
	}
	return nil
}

func (c *Client) runWithPostFields(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	if err := writer.WriteField("query", req.q); err != nil {
		return errors.Wrap(err, "write query field")
	}
	var variablesBuf bytes.Buffer
	if len(req.vars) > 0 {
		variablesField, err := writer.CreateFormField("variables")
		if err != nil {
			return errors.Wrap(err, "create variables field")
		}
		if err := json.NewEncoder(io.MultiWriter(variablesField, &variablesBuf)).Encode(req.vars); err != nil {
			return errors.Wrap(err, "encode variables")
		}
	}
	for i := range req.files {
		part, err := writer.CreateFormFile(req.files[i].Field, req.files[i].Name)
		if err != nil {
			return errors.Wrap(err, "create form file")
		}
		if _, err := io.Copy(part, req.files[i].R); err != nil {
			return errors.Wrap(err, "preparing file")
		}
	}
	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "close writer")
	}
	c.logf(">> variables: %s", variablesBuf.String())
	c.logf(">> files: %d", len(req.files))
	c.logf(">> query: %s", req.q)

	req.body = requestBody
	req.contentType = writer.FormDataContentType()

	return c.makeRequest(ctx, req, resp)
}

func (c *Client) runMultipartRequestSpec(ctx context.Context, req *Request, resp interface{}) error {

	if len(req.vars) > 0 {
		return errors.New("variables doesn't supported due to the multipart request spec https://github.com/jaydenseric/graphql-multipart-request-spec/issues/22")
	}

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	multipartRequestSpecQuery := req.fillMultipartRequestSpecQuery()
	operations, err := json.Marshal(multipartRequestSpecQuery.Operations)
	if err != nil {
		return errors.Wrap(err, "marshal operations")
	}
	maps, err := json.Marshal(multipartRequestSpecQuery.Map)
	if err != nil {
		return errors.Wrap(err, "marshal map")
	}

	if err := writer.WriteField("operations", string(operations)); err != nil {
		return errors.Wrap(err, "write operation field")
	} else {
		c.logf(">> field: %s = %s", "operations", string(operations))
	}

	if err := writer.WriteField("map", string(maps)); err != nil {
		return errors.Wrap(err, "write maps field")
	} else {
		c.logf(">> field: %s = %s", "map", string(maps))
	}

	for i := range req.files {
		part, err := writer.CreateFormFile(req.files[i].Field, req.files[i].Name)
		if err != nil {
			return errors.Wrap(err, "create form file")
		}
		if _, err := io.Copy(part, req.files[i].R); err != nil {
			return errors.Wrap(err, "preparing file")
		}

		fieldName := req.files[i].Field
		fieldValue := `@` + req.files[i].Name

		if err := writer.WriteField(fieldName, fieldValue); err != nil {
			return errors.Wrap(err, "write maps field")
		} else {
			c.logf(">> field: %s = %s", fieldName, fieldValue)
		}
	}
	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "close writer")
	}

	req.body = requestBody
	req.contentType = writer.FormDataContentType()

	return c.makeRequest(ctx, req, resp)
}

func (c *Client) makeRequest(ctx context.Context, req *Request, resp interface{}) error {
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &req.body)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", req.contentType)
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	c.logf(">> headers: %v", r.Header)
	r = r.WithContext(ctx)
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return errors.Wrap(err, "reading body")
	}
	c.logf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return errors.Wrap(err, "decoding response")
	}
	if len(gr.Errors) > 0 {
		return gr.Errors
	}
	return nil
}

type multipartRequestSpecQuery struct {
	Operations struct {
		Query     string      `json:"query"`
		Variables interface{} `json:"variables"`
	} `json:"operations"`
	Map map[string][]string `json:"map"`
}

func (req *Request) fillMultipartRequestSpecQuery() multipartRequestSpecQuery {
	type fileVariables struct {
		File interface{} `json:"file"`
	}
	type filesVariables struct {
		Files []interface{} `json:"files"`
	}
	type emptyVariables struct {
	}

	query := new(multipartRequestSpecQuery)
	query.Operations.Query = req.Query()
	query.Map = make(map[string][]string)

	switch c := len(req.Files()); {
	default:
		fallthrough
	case c == 0:
		query.Operations.Variables = new(emptyVariables)
		return *query
	case c == 1:
		variables := new(fileVariables)
		variables.File = nil
		query.Map[req.Files()[0].Field] = []string{`variables.file`}
		query.Operations.Variables = variables
		return *query
	case c > 0:
		variables := new(filesVariables)
		for index, file := range req.Files() {
			variables.Files = append(variables.Files, nil)
			query.Map[file.Field] = []string{`variables.files.` + strconv.Itoa(index)}
		}
		query.Operations.Variables = variables
		return *query
	}
}

// WithHTTPClient specifies the underlying http.Client to use when
// making requests.
//  NewClient(endpoint, WithHTTPClient(specificHTTPClient))
func WithHTTPClient(httpclient *http.Client) ClientOption {
	return func(client *Client) {
		client.httpClient = httpclient
	}
}

// UseMultipartForm uses multipart/form-data and activates support for
// files.
func UseMultipartForm() ClientOption {
	return func(client *Client) {
		client.useMultipartForm = true
	}
}

// UseMultipartRequestSpec uses for files upload, implementing multipart request specification:
// https://github.com/jaydenseric/graphql-multipart-request-spec
// Variables doesn't supported: https://github.com/jaydenseric/graphql-multipart-request-spec/issues/22
func UseMultipartRequestSpec() ClientOption {
	return func(client *Client) {
		client.useMultipartRequestSpec = true
	}
}

//ImmediatelyCloseReqBody will close the req body immediately after each request body is ready
func ImmediatelyCloseReqBody() ClientOption {
	return func(client *Client) {
		client.closeReq = true
	}
}

// ClientOption are functions that are passed into NewClient to
// modify the behaviour of the Client.
type ClientOption func(*Client)

// Error represents a GraphQL error
type Error struct {
	Message   string
	Locations []struct {
		Line   int
		Column int
	}
	Path       []interface{}
	Extensions map[string]interface{}
}

// Error implements error interface
func (e Error) Error() string {
	return fmt.Sprintf("graphql: %s", e.Message)
}

// Errors represents a list of GraphQL errors
type Errors []Error

// Error implements error interface
func (l Errors) Error() string {
	if len(l) == 0 {
		return "no error"
	}

	result := make([]string, len(l))
	for i, e := range l {
		result[i] = e.Message
	}

	return fmt.Sprintf("graphql: %s", strings.Join(result, " | "))
}

type graphResponse struct {
	Data   interface{}
	Errors Errors
}

// Request is a GraphQL request.
type Request struct {
	q     string
	vars  map[string]interface{}
	files []File

	// Header represent any request headers that will be set
	// when the request is made.
	Header http.Header

	body        bytes.Buffer
	contentType string
}

// NewRequest makes a new Request with the specified string.
func NewRequest(q string) *Request {
	req := &Request{
		q:      q,
		Header: make(map[string][]string),
	}
	return req
}

// Var sets a variable.
func (req *Request) Var(key string, value interface{}) {
	if req.vars == nil {
		req.vars = make(map[string]interface{})
	}
	req.vars[key] = value
}

// Vars gets the variables for this Request.
func (req *Request) Vars() map[string]interface{} {
	return req.vars
}

// Files gets the files in this request.
func (req *Request) Files() []File {
	return req.files
}

// Query gets the query string of this request.
func (req *Request) Query() string {
	return req.q
}

// File sets a file to upload.
// Files are only supported with a Client that was created with
// the UseMultipartForm option.
func (req *Request) File(fieldname, filename string, r io.Reader) {
	req.files = append(req.files, File{
		Field: fieldname,
		Name:  filename,
		R:     r,
	})
}

// File represents a file to upload.
type File struct {
	Field string
	Name  string
	R     io.Reader
}

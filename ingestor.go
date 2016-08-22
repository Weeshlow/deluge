package deluge

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/unchartedsoftware/plog"
	es "gopkg.in/olivere/elastic.v3"

	"github.com/unchartedsoftware/deluge/elastic"
	"github.com/unchartedsoftware/deluge/elastic/equalizer"
	"github.com/unchartedsoftware/deluge/pool"
	"github.com/unchartedsoftware/deluge/threshold"
	"github.com/unchartedsoftware/deluge/util/progress"
)

const (
	defaultHost                 = "localhost"
	defaultPort                 = "9200"
	defaultClearExisting        = true
	defaultNumActiveConnections = 8
	defaultNumWorkers           = 8
	defaultCompression          = ""
	defaultNumReplicas          = 1
	defaultThreshold            = 0.01
	defaultBulkByteSize         = 1024 * 1024 * 20
)

// Ingestor is an Elasticsearch ingestor client. Create one by calling
// NewIngestor.
type Ingestor struct {
	input                Input
	document             Document
	index                string
	host                 string
	port                 string
	clearExisting        bool
	numActiveConnections int
	numWorkers           int
	numReplicas          int
	compression          string
	threshold            float64
	bulkByteSize         int64
}

// NewIngestor instantiates and configures a new Ingestor instance.
func NewIngestor(options ...IngestorOptionFunc) (*Ingestor, error) {
	// Set up the ingestor
	i := &Ingestor{
		host:                 defaultHost,
		port:                 defaultPort,
		clearExisting:        defaultClearExisting,
		compression:          defaultCompression,
		numActiveConnections: defaultNumActiveConnections,
		numWorkers:           defaultNumWorkers,
		numReplicas:          defaultNumReplicas,
		threshold:            defaultThreshold,
		bulkByteSize:         defaultBulkByteSize,
	}
	// Run the options through it
	for _, option := range options {
		if err := option(i); err != nil {
			return nil, err
		}
	}
	return i, nil
}

// Ingest will run the ingest job.
func (i *Ingestor) Ingest() error {

	// check that we have the required options set
	if i.index == "" {
		return fmt.Errorf("Ingestor `index` has not been set with SetIndex() option")
	}
	if i.document == nil {
		return fmt.Errorf("Ingestor `document` has not been set with SetDocument() option")
	}
	if i.input == nil {
		return fmt.Errorf("Ingestor `input` has not been set with SetInput() option")
	}

	// print input summary
	log.Info(i.input.Summary())

	// get the document mapping
	mapping, err := i.document.GetMapping()
	if err != nil {
		return err
	}

	// prepare elasticsearch index
	err = elastic.PrepareIndex(
		i.host,
		i.port,
		i.index,
		mapping,
		i.clearExisting)
	if err != nil {
		return err
	}

	// open the backpressure equalizer
	equalizer.Open(i.numActiveConnections)

	// create pool of size N
	p := pool.New(i.numActiveConnections)

	// start progress tracking
	progress.StartProgress()

	// launch the ingest job
	err = p.Execute(i.newlineWorker(), i.input)

	// log errors
	errs := threshold.Errs()
	if len(errs) > 0 {
		log.Errorf("Failed ingesting %d documents", len(errs))
		for _, err := range threshold.Errs() {
			log.Error(err)
		}
	}

	if err != nil {
		// error threshold was surpassed, or there was a fatal error
		// otherwise the pool would not return this error
		progress.EndProgress()
		progress.PrintFailure()
		return err
	}

	// success
	progress.EndProgress()
	progress.PrintSuccess()

	// close the backpressure equalizer
	equalizer.Close()

	// enable replication
	if i.numReplicas > 0 {
		err := elastic.EnableReplicas(
			i.host,
			i.port,
			i.index,
			i.numReplicas)
		if errs != nil {
			return err
		}
	}
	return nil
}

// Workers

func getReader(reader io.Reader, compression string) (io.Reader, error) {
	// use compression based reader if specified
	switch compression {
	case "gzip":
		return gzip.NewReader(reader)
	default:
		return reader, nil
	}
}

func (i *Ingestor) newRequest() (*equalizer.Request, error) {
	return equalizer.NewRequest(
		i.host,
		i.port,
		i.index)
}

func (i *Ingestor) newBulkIndexRequest(line string) (*es.BulkIndexRequest, error) {
	// set data for document
	err := i.document.SetData(line)
	if err != nil {
		return nil, err
	}
	// get id from document
	id, err := i.document.GetID()
	if err != nil {
		return nil, err
	}
	// get type from document
	typ, err := i.document.GetType()
	if err != nil {
		return nil, err
	}
	// get source from document
	source, err := i.document.GetSource()
	if err != nil {
		return nil, err
	}
	if source != nil {
		// create index action
		return es.NewBulkIndexRequest().
			Id(id).
			Type(typ).
			Doc(source), nil
	}
	return nil, nil
}

func (i *Ingestor) newlineWorker() pool.Worker {
	return func(next io.Reader) error {

		// get decompress reader (if compression is specified / supported)
		reader, err := getReader(next, i.compression)
		if threshold.CheckErr(err, i.threshold) {
			return err
		}

		// scan file line by line
		scanner := bufio.NewScanner(reader)

		// total bytes sent
		bytes := int64(0)

		for {
			// create a new bulk request object
			bulk, err := i.newRequest()
			if err != nil {
				return err
			}

			// begin reading file, line by line
			for scanner.Scan() {

				// read line of file
				line := scanner.Text()

				// create bulk index request
				req, err := i.newBulkIndexRequest(line)
				if threshold.CheckErr(err, i.threshold) {
					return err
				}

				// ensure that the request was created
				if req != nil {
					// add index action to bulk req
					bulk.Add(req)
					// flag this document as successful
					threshold.AddSuccess()
					// check if we have hit batch size limit
					if bulk.EstimatedSizeInBytes() >= i.bulkByteSize {
						// ready to send
						break
					}
				}
			}

			// if no actions, we are finished
			if bulk.NumberOfActions() == 0 {
				break
			}

			// add total bytes
			bytes += int64(bulk.EstimatedSizeInBytes())

			// send the request through the equalizer, this will wait until the
			// equalizer determines ES is 'ready'.
			// NOTE: Due to the asynchronous nature of the equalizer, error
			// values returned here may not be caused from this worker
			// goroutine.
			err = equalizer.Send(bulk)
			if err != nil {
				// add error to internal slice
				threshold.CheckErr(err, i.threshold)
				// always return on bulk ingest error
				return err
			}
			// update and print current progress
			// NOTE: Due to the asynchronous nature of the equalizer, the
			// request sent from this worker may not have actually been ingested
			// by this time. However updating the progress with this workers
			// payload size still gives a relatively accurate progress.
			progress.UpdateProgress(bytes)

		}
		return nil
	}
}

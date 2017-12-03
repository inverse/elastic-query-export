package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"time"

	"context"
	"fmt"
	"encoding/csv"

	"github.com/jawher/mow.cli"
	"golang.org/x/sync/errgroup"
	"gopkg.in/cheggaaa/pb.v1"
	"gopkg.in/olivere/elastic.v5"
	"strings"
)

var Version string

func main() {

	app := cli.App("es-query-csv", "CLI tool to export data from ElasticSearch into a CSV file.")
	app.Version("v version", Version)

	var (
		configElasticURL = app.StringOpt("e eshost", "http://localhost:9200", "ElasticSearch URL")
		configIndex = app.StringOpt("i index", "logs-*", "ElasticSearch Index (or Index Prefix)")
		configRawQuery = app.StringOpt("r rawquery", "", "ElasticSearch Raw Querystring")
		configQuery = app.StringOpt("q query", "", "Lucene Query like in Kibana search input")
		configOutfile = app.StringOpt("o outfile", "output.csv", "Filepath for CSV output")
		configFieldlist = app.StringOpt("fields", "", "Fields to include in export as comma separated list")
		configFields = app.StringsOpt("f field", nil, "Field to include in export, can be added multiple for every field")
	)

	app.Action = func() {
		client, err := elastic.NewClient(
			elastic.SetURL(*configElasticURL),
			elastic.SetSniff(false),
			elastic.SetHealthcheckInterval(60*time.Second),
			elastic.SetErrorLog(log.New(os.Stderr, "ELASTIC ", log.LstdFlags)),
		)
		if err != nil {
			panic(err)
		}

		if *configFieldlist != "" {
			*configFields = strings.Split(*configFieldlist,",")
		}

		outfile, err := os.Create(*configOutfile)
		if err != nil {
			panic(err)
		}
		defer outfile.Close()

		g, ctx := errgroup.WithContext(context.Background())

		var esQuery elastic.Query
		if *configRawQuery != "" {
			esQuery = elastic.NewRawStringQuery(*configQuery)
		} else if *configQuery != "" {
			esQuery = elastic.NewQueryStringQuery(*configQuery)
		} else {
			esQuery = elastic.NewMatchAllQuery()
		}

		// Count total and setup progress
		total, err := client.Count(*configIndex).Query(esQuery).Do(ctx)
		if err != nil {
			panic(err)
		}
		bar := pb.StartNew(int(total))

		// one goroutine to receive hits from Elastic and send them to hits channel
		hits := make(chan json.RawMessage)
		g.Go(func() error {
			defer close(hits)

			scroll := client.Scroll(*configIndex).Size(100).Query(esQuery)

			// include selected fields otherwise export all
			if *configFields != nil {
				fetchSource := elastic.NewFetchSourceContext(true)
				for _, field := range *configFields {
					fetchSource.Include(field)
				}
				scroll = scroll.FetchSourceContext(fetchSource)
			}

			for {
				results, err := scroll.Do(ctx)
				if err == io.EOF {
					return nil // all results retrieved
				}
				if err != nil {
					return err // something went wrong
				}

				// Send the hits to the hits channel
				for _, hit := range results.Hits.Hits {
					hits <- *hit.Source
				}

				// Check if we need to terminate early
				select {
				default:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})

		// goroutine outside of the errgroup to receive csv outputs from csvout channel and write to file
		csvout := make(chan []string, 5)
		go func() {

			w := csv.NewWriter(outfile)

			var csvheader []string
			for _, field := range *configFields {
				csvheader = append(csvheader, field)
			}
			if err := w.Write(csvheader); err != nil {
				fmt.Printf("Error: %v\n", err)
			}

			for csvdata := range csvout {

				if err := w.Write(csvdata); err != nil {
					fmt.Printf("Error: %v\n", err)
				}

				w.Flush()

				bar.Increment()
			}

		}()

		// some more goroutines in the errgroup context to do the transformation, room to add more work here in future
		for i := 0; i < 5; i++ {
			g.Go(func() error {
				var document map[string]interface{}

				for hit := range hits {

					var csvdata []string

					if err := json.Unmarshal(hit, &document); err != nil {
						fmt.Printf("Error: %v\n", err)
					}

					for _, field := range *configFields {
						csvdata = append(csvdata, fmt.Sprintf("%v", document[field]))
					}

					// send string array to csv output
					csvout <- csvdata

					select {
					default:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
		}

		// Check if any goroutines failed.
		if err := g.Wait(); err != nil {
			panic(err)
		}

		bar.FinishPrint("Done")
	}

	app.Run(os.Args)
}
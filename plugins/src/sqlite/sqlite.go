package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sync"

	"github.com/blastrain/vitess-sqlparser/sqlparser"
	"github.com/cert-lv/graphoscope/pdk"
	_ "github.com/mattn/go-sqlite3"
	"github.com/umpc/go-sortedmap"
	"github.com/umpc/go-sortedmap/desc"
)

/*
 * Check "pdk/plugin.go" for the built-in plugin functions description
 */

func (p *plugin) Source() *pdk.Source {
	return p.source
}

func (p *plugin) Setup(source *pdk.Source, limit int) error {

	// Validate necessary parameters
	if source.Access["db"] == "" {
		return fmt.Errorf("'access.db' is not defined")
	} else if source.Access["table"] == "" {
		return fmt.Errorf("'access.table' is not defined")
	}

	db, err := sql.Open("sqlite3", source.Access["db"])
	if err != nil {
		return err
	}

	// Store settings
	p.source = source
	p.db = db
	p.limit = limit

	// Set possible variable type & searching fields
	for _, relation := range source.Relations {
		for _, types := range relation.From.VarTypes {
			types.RegexCompiled = regexp.MustCompile(types.Regex)
		}

		for _, types := range relation.To.VarTypes {
			types.RegexCompiled = regexp.MustCompile(types.Regex)
		}
	}

	//fmt.Printf("SQLite %s: %#v\n\n", source.Name, p)
	return nil
}

func (p *plugin) Search(stmt *sqlparser.Select) ([]map[string]interface{}, map[string]interface{}, error) {

	// Storage for the results to return
	results := []map[string]interface{}{}

	// Convert SQL statement
	query, err := p.convert(stmt)
	if err != nil {
		return nil, nil, err
	}

	/*
	 * Run the query
	 */

	// Context to be able to cancel goroutines
	// when some DB wants to return > limit amount of entries
	// or time expires
	ctx, cancel := context.WithTimeout(context.Background(), p.source.Timeout)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, "SELECT * FROM "+p.source.Access["table"]+" WHERE "+query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	var row = make([]interface{}, 0, len(cols))
	for range cols {
		row = append(row, new(string))
	}

	mx := sync.Mutex{}
	umx := sync.Mutex{}
	unique := make(map[string]bool)
	counter := 0

	// Struct to store statistics data
	// when the amount of returned entries is too large
	stats := pdk.NewStats()

	for _, field := range p.source.StatsFields {
		stats.Fields[field] = sortedmap.New(10, desc.Int)
	}

	/*
	 * Iterate through the results
	 */

	for rows.Next() {

		// Stop when results count is too big
		if counter >= p.limit {
			cancel()

			top, err := stats.ToJSON(p.source.Name)
			if err != nil {
				return nil, nil, err
			}

			return nil, top, nil
		}

		if err := rows.Scan(row...); err != nil {
			return nil, nil, err
		}

		// Deserialize
		entry := make(map[string]interface{})

		for i, col := range cols {
			entry[col] = *row[i].(*string)
		}

		// Update stats
		for _, field := range p.source.StatsFields {
			stats.Update(entry, field)
		}

		// Go through all the predefined relations and collect unique entries
		for _, relation := range p.source.Relations {
			if entry[relation.From.ID] != nil && entry[relation.To.ID] != nil {
				umx.Lock()
				if _, exists := unique[entry[relation.From.ID].(string)+entry[relation.To.ID].(string)]; exists {
					umx.Unlock()
					continue
				}

				counter++

				unique[entry[relation.From.ID].(string)+entry[relation.To.ID].(string)] = true
				umx.Unlock()

				/*
				 * FROM node with attributes
				 */
				from := map[string]interface{}{
					"id":     entry[relation.From.ID],
					"group":  relation.From.Group,
					"search": relation.From.Search,
				}

				// Check FROM type & searching fields
				if len(relation.From.VarTypes) > 0 {
					for _, t := range relation.From.VarTypes {
						if t.RegexCompiled.MatchString(entry[relation.From.ID].(string)) {
							from["group"] = t.Group
							from["search"] = t.Search

							break
						}
					}
				}

				if len(relation.From.Attributes) > 0 {
					from["attributes"] = make(map[string]interface{})
					pdk.CopyPresentValues(entry, from["attributes"].(map[string]interface{}), relation.From.Attributes)
				}

				/*
				 * TO node
				 */
				to := map[string]interface{}{
					"id":     entry[relation.To.ID],
					"group":  relation.To.Group,
					"search": relation.To.Search,
				}

				// Check FROM type & searching fields
				if len(relation.To.VarTypes) > 0 {
					for _, t := range relation.To.VarTypes {
						if t.RegexCompiled.MatchString(entry[relation.To.ID].(string)) {
							to["group"] = t.Group
							to["search"] = t.Search

							break
						}
					}
				}

				if len(relation.To.Attributes) > 0 {
					to["attributes"] = make(map[string]interface{})
					pdk.CopyPresentValues(entry, to["attributes"].(map[string]interface{}), relation.To.Attributes)
				}

				// Resulting graph entry to return
				result := make(map[string]interface{})

				/*
				 * Edge between FROM and TO
				 */
				if relation.Edge != nil && (relation.Edge.Label != "" || len(relation.Edge.Attributes) > 0) {
					result["edge"] = make(map[string]interface{})

					if relation.Edge.Label != "" {
						result["edge"].(map[string]interface{})["label"] = relation.Edge.Label
					}

					if len(relation.Edge.Attributes) > 0 {
						pdk.CopyPresentValues(entry, result["edge"].(map[string]interface{}), relation.Edge.Attributes)
					}
				}

				/*
				 * Put it together
				 */
				result["from"] = from
				result["to"] = to
				result["source"] = p.source.Name

				//fmt.Println("Edge:", from, to)

				/*
				 * Add current entry to the list to return
				 */
				mx.Lock()
				results = append(results, result)
				mx.Unlock()
			}
		}
	}

	err = rows.Err()
	if err != nil {
		return nil, nil, err
	}

	return results, nil, nil
}

func (p *plugin) Stop() error {
	return p.db.Close()
}

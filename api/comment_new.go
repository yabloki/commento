package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Take `creationDate` as a param because comment import (from Disqus, for
// example) will require a custom time.
func commentNew(commenterHex string, domain string, path string, parentHex string, markdown string, state string, postID string, commentPrice uint64, creationDate time.Time) (string, error) {
	// path is allowed to be empty
	if commenterHex == "" || domain == "" || parentHex == "" || markdown == "" || state == "" || postID == "" {
		return "", errorMissingField
	}
	p, err := pageGet(domain, path)
	if err != nil {
		logger.Errorf("cannot get page attributes: %v", err)
		return "", errorInternal
	}

	if p.IsLocked {
		return "", errorThreadLocked
	}

	commentHex, err := randomHex(32)
	if err != nil {
		return "", err
	}

	html := markdownToHtml(markdown)

	if err = pageNew(domain, path); err != nil {
		return "", err
	}

	statement := `
		INSERT INTO
		comments (commentHex, domain, path, postID, commenterHex, parentHex, markdown, html, creationDate, state)
		VALUES   ($1,         $2,     $3,     $10,       $4,         $5,        $6,     $7,      $8,         $9 );
	`
	_, err = db.Exec(statement, commentHex, domain, path, commenterHex, parentHex, markdown, html, creationDate, state, postID)
	if err != nil {
		logger.Errorf("cannot insert comment: %v", err)
		return "", errorInternal
	}

	statement = `
		UPDATE commenters
		SET cnttokens = cnttokens - $1::bigint
		WHERE CommenterHex = $2
		`
	_, err = db.Exec(statement, commentPrice, commenterHex)

	if err != nil {
		logger.Errorf("error updating CNT tokens in comments: %v", err)
		return "", errorInternal
	}

	return commentHex, nil
}

func getCommentPrice(postID string) (uint64, error) {
	message := map[string]interface{}{
		"postId": postID,
	}

	bytesRepresentation, err := json.Marshal(message)
	if err != nil {
		logger.Errorf("error marshalizing postid payload json: %v", err)
		return 0, errorInternal
	}

	//TODO probably need to use something like os.Getenv("ORIGIN")
	resp, err := http.Post("http://core.2cents.media/post", "application/json", bytes.NewBuffer(bytesRepresentation))
	if err != nil {
		logger.Errorf("error getting price of comment: %v", err)
		return 0, errorInternal
	}

	var result map[string]string

	json.NewDecoder(resp.Body).Decode(&result)
	i, err := strconv.Atoi(result["price"])
	if err != nil {
		logger.Errorf("error parsing price: %v", err)
		return 0, errorInternal
	}
	return uint64(i), nil
}

func commentNewHandler(w http.ResponseWriter, r *http.Request) {
	type request struct {
		CommenterToken *string `json:"commenterToken"`
		Domain         *string `json:"domain"`
		Path           *string `json:"path"`
		PostID         *string `json:"postId"`
		ParentHex      *string `json:"parentHex"`
		Markdown       *string `json:"markdown"`
	}

	var x request
	if err := bodyUnmarshal(r, &x); err != nil {
		bodyMarshal(w, response{"success": false, "message": err.Error()})
		return
	}

	domain := domainStrip(*x.Domain)
	path := *x.Path

	d, err := domainGet(domain)
	if err != nil {
		bodyMarshal(w, response{"success": false, "message": err.Error()})
		return
	}

	if d.State == "frozen" {
		bodyMarshal(w, response{"success": false, "message": errorDomainFrozen.Error()})
		return
	}

	if d.RequireIdentification && *x.CommenterToken == "anonymous" {
		bodyMarshal(w, response{"success": false, "message": errorNotAuthorised.Error()})
		return
	}

	// logic: (empty column indicates the value doesn't matter)
	// | anonymous | moderator | requireIdentification | requireModeration | moderateAllAnonymous | approved? |
	// |-----------+-----------+-----------------------+-------------------+----------------------+-----------|
	// |       yes |           |                       |                   |                   no |       yes |
	// |       yes |           |                       |                   |                  yes |        no |
	// |        no |       yes |                       |                   |                      |       yes |
	// |        no |        no |                       |               yes |                      |       yes |
	// |        no |        no |                       |                no |                      |        no |

	var commenterHex string
	var commentPriceWrapper uint64
	var state string

	if *x.CommenterToken == "anonymous" {
		commenterHex = "anonymous"
		if isSpam(*x.Domain, getIp(r), getUserAgent(r), "Anonymous", "", "", *x.Markdown) {
			state = "flagged"
		} else {
			if d.ModerateAllAnonymous || d.RequireModeration {
				state = "unapproved"
			} else {
				state = "approved"
			}
		}
	} else {
		c, err := commenterGetByCommenterToken(*x.CommenterToken)
		if err != nil {
			bodyMarshal(w, response{"success": false, "message": err.Error()})
			return
		}

		commentPrice, err := getCommentPrice(*x.PostID)
		if err != nil {
			bodyMarshal(w, response{"success": false, "message": err.Error()})
			return
		}
		// i don't know what is going on with scope in golang, leave me alone
		commentPriceWrapper = commentPrice

		if c.CNTTokensAmount < commentPrice {
			bodyMarshal(w, response{"success": false, "message": errorNoCNT.Error()})
			return
		}

		// cheaper than a SQL query as we already have this information
		isModerator := false
		for _, mod := range d.Moderators {
			if mod.Email == c.Email {
				isModerator = true
				break
			}
		}

		commenterHex = c.CommenterHex

		if isModerator {
			state = "approved"
		} else {
			if isSpam(*x.Domain, getIp(r), getUserAgent(r), c.Name, c.Email, c.Link, *x.Markdown) {
				state = "flagged"
			} else {
				if d.RequireModeration {
					state = "unapproved"
				} else {
					state = "approved"
				}
			}
		}
	}
	commentHex, err := commentNew(commenterHex, domain, path, *x.ParentHex, *x.Markdown, state, *x.PostID, commentPriceWrapper, time.Now().UTC())
	if err != nil {
		bodyMarshal(w, response{"success": false, "message": err.Error()})
		return
	}

	// TODO: reuse html in commentNew and do only one markdown to HTML conversion?
	html := markdownToHtml(*x.Markdown)

	bodyMarshal(w, response{"success": true, "commentHex": commentHex, "state": state, "html": html})
	if smtpConfigured {
		go emailNotificationNew(d, path, commenterHex, commentHex, html, *x.ParentHex, state)
	}
}

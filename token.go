package main

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	log "github.com/sirupsen/logrus"
)

type token struct {
	Code   string `json:"code"`
	Icon   string `json:"icon"`
	Name   string `json:"name"`
	Secret string `json:"-"`
	Digits int    `json:"digits"`
	Period int    `json:"period"`
}

func (t *token) GenerateCode(next bool) error {
	secret := t.Secret

	if n := len(secret) % 8; n != 0 {
		secret = secret + strings.Repeat("=", 8-n)
	}

	opts := totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}

	if t.Digits != 0 {
		opts.Digits = otp.Digits(t.Digits)
	}

	if t.Period != 0 {
		opts.Period = uint(t.Period)
	}

	var pointOfTime = time.Now()
	if next {
		pointOfTime = pointOfTime.Add(time.Duration(t.Period) * time.Second)
	}

	var err error
	t.Code, err = totp.GenerateCodeCustom(strings.ToUpper(secret), pointOfTime, opts)
	return err
}

// Sorter interface

type tokenList []*token

func (t tokenList) Len() int           { return len(t) }
func (t tokenList) Less(i, j int) bool { return strings.ToLower(t[i].Name) < strings.ToLower(t[j].Name) }
func (t tokenList) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }

func (t tokenList) LongestName() (l int) {
	for _, s := range t {
		if ll := len(s.Name); ll > l {
			l = ll
		}
	}

	return
}

func (t tokenList) MinPeriod() int {
	var m int = math.MaxInt32

	for _, tok := range t {
		if tok.Period != 0 && tok.Period < m {
			m = tok.Period
		}
	}

	if m == math.MaxInt32 {
		// Fallback: Everything uses the default value
		m = 30
	}

	return m
}

func useOrRenewToken(tok, accessToken string) (string, error) {
	client, err := api.NewClient(&api.Config{
		Address: cfg.Vault.Address,
	})

	if err != nil {
		return "", fmt.Errorf("Unable to create client: %s", err)
	}

	if tok != "" {
		client.SetToken(tok)
		s, err := client.Auth().Token().LookupSelf()
		if err == nil && s.Data != nil {
			log.WithFields(log.Fields{"token": hashSecret(tok)}).Debugf("Token is valid for another %vs", s.Data["ttl"])
			return tok, nil
		}

		log.WithFields(log.Fields{"token": hashSecret(tok)}).Debugf("Token did not met requirements: err = %s", err)
		if s != nil {
			log.WithFields(log.Fields{"token": hashSecret(tok)}).Debugf("Token did not met requirements: data = %v", s.Data)
		}
	}

	s, err := client.Logical().Write("auth/github/login", map[string]interface{}{"token": accessToken})
	if err != nil || s.Auth == nil {
		return "", fmt.Errorf("Login did not work: Error = %s", err)
	}
	return s.Auth.ClientToken, nil
}

func getSecretsFromVault(tok string, next bool) ([]*token, error) {
	client, err := api.NewClient(&api.Config{
		Address: cfg.Vault.Address,
	})

	if err != nil {
		return nil, fmt.Errorf("Unable to create client: %s", err)
	}

	client.SetToken(tok)

	key := cfg.Vault.Prefix

	resp := []*token{}
	respChan := make(chan *token, 100)

	keyPoolChan := make(chan string, 100)

	scanPool := make(chan string, 100)
	scanPool <- strings.TrimRight(key, "*")

	done := make(chan struct{})
	defer func() { done <- struct{}{} }()

	wg := new(sync.WaitGroup)
	wg.Add(1)

	go func() {
		for {
			select {
			case key := <-scanPool:
				go scanKeyForSubKeys(client, key, scanPool, keyPoolChan, wg)
			case key := <-keyPoolChan:
				go fetchTokenFromKey(client, key, respChan, wg, next)
			case t := <-respChan:
				resp = append(resp, t)
				wg.Done()
			case <-done:
				close(scanPool)
				close(keyPoolChan)
				close(respChan)
				return
			}
		}
	}()

	wg.Wait()

	sort.Sort(tokenList(resp))

	return resp, nil
}

func scanKeyForSubKeys(client *api.Client, key string, subKeyChan, tokenKeyChan chan string, wg *sync.WaitGroup) {
	defer wg.Done()

	s, err := client.Logical().List(key)
	if err != nil {
		log.Errorf("Unable to list keys %q: %s", key, err)
		return
	}

	if s == nil {
		log.Errorf("There is no key %q", key)
		return
	}

	if s.Data["keys"] != nil {
		for _, sk := range s.Data["keys"].([]interface{}) {
			sks := sk.(string)
			if strings.HasSuffix(sks, "/") {
				wg.Add(1)
				subKeyChan <- path.Join(key, sks)
			} else {
				wg.Add(1)
				tokenKeyChan <- path.Join(key, sks)
			}
		}
	}
}

func fetchTokenFromKey(client *api.Client, k string, respChan chan *token, wg *sync.WaitGroup, next bool) {
	defer wg.Done()

	data, err := client.Logical().Read(k)
	if err != nil {
		log.Errorf("Unable to read from key %q: %s", k, err)
		return
	}

	if data.Data == nil {
		// Key without any data? Weird.
		return
	}

	tok := &token{
		Icon: "key",
		Name: k,
	}

	for k, v := range data.Data {
		switch k {
		case cfg.Vault.SecretField:
			tok.Secret = v.(string)
		case "code":
			tok.Code = v.(string)
		case "name":
			tok.Name = v.(string)
		case "account_name":
			tok.Name = v.(string)
		case "icon":
			tok.Icon = v.(string)
		case "digits":
			tok.Digits, err = strconv.Atoi(v.(string))
			if err != nil {
				log.WithError(err).Error("Unable to parse digits")
			}
		case "period":
			tok.Period, err = strconv.Atoi(v.(string))
			if err != nil {
				log.WithError(err).Error("Unable to parse digits")
			}
		}
	}

	if err = tok.GenerateCode(next); err != nil {
		log.WithError(err).WithField("name", tok.Name).Error("Unable to generate code")
		return
	}

	if tok.Code == "" {
		// Nothing ended in us having a code, does not seem to be something for us
		return
	}

	wg.Add(1)
	respChan <- tok
}

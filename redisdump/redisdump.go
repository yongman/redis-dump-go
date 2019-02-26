package redisdump

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	radix "github.com/mediocregopher/radix.v3"
)

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func ttlToRedisCmd(k string, val int64) []string {
	return []string{"EXPIREAT", k, fmt.Sprint(time.Now().Unix() + val)}
}

func stringToRedisCmd(k, val string) []string {
	return []string{"SET", k, val}
}

func hashToRedisCmd(k string, val map[string]string) []string {
	cmd := []string{"HSET", k}
	for k, v := range val {
		cmd = append(cmd, k, v)
	}
	return cmd
}

func setToRedisCmd(k string, val []string) []string {
	cmd := []string{"SADD", k}
	return append(cmd, val...)
}

func listToRedisCmd(k string, val []string) []string {
	cmd := []string{"RPUSH", k}
	return append(cmd, val...)
}

func zsetToRedisCmd(k string, val []string) []string {
	cmd := []string{"ZADD", k}
	var key string

	for i, v := range val {
		if i%2 == 0 {
			key = v
			continue
		}

		cmd = append(cmd, v, key)
	}
	return cmd
}

// RESPSerializer will serialize cmd to RESP
func RESPSerializer(cmd []string) string {
	s := ""
	s += "*" + strconv.Itoa(len(cmd)) + "\r\n"
	for _, arg := range cmd {
		s += "$" + strconv.Itoa(len(arg)) + "\r\n"
		s += arg + "\r\n"
	}
	return s
}

// RedisCmdSerializer will serialize cmd to a string with redis commands
func RedisCmdSerializer(cmd []string) string {
	return strings.Join(cmd, " ")
}

func RedisKeysSerializer(keys []string) string {
	if len(keys) > 0 && keys[0] == "SELECT" {
		return ""
	}
	return strings.Join(keys, "\n")
}

func dumpKeys(client radix.Client, keys []string, withTTL bool, logger *log.Logger, key_regexp string, serializer func([]string) string) error {
	var err error
	var redisCmd []string

	if reflect.ValueOf(serializer).Pointer() == reflect.ValueOf(RedisKeysSerializer).Pointer() {
		pattern := regexp.MustCompile(key_regexp)

		filtered_keys := []string{}
		for _, key := range keys {
			if pattern.MatchString(key) {
				filtered_keys = append(filtered_keys, key)
			}
		}
		if len(filtered_keys) > 0 {
			fmt.Fprint(os.Stdout, RedisKeysSerializer(filtered_keys))
			fmt.Fprint(os.Stdout, "\n")
		}
		return nil
	}

	for _, key := range keys {

		var keyType string

		err = client.Do(radix.Cmd(&keyType, "TYPE", key))
		if err != nil {
			return err
		}

		switch keyType {
		case "string":
			var val string
			if err = client.Do(radix.Cmd(&val, "GET", key)); err != nil {
				return err
			}
			redisCmd = stringToRedisCmd(key, val)

		case "list":
			var val []string
			if err = client.Do(radix.Cmd(&val, "LRANGE", key, "0", "-1")); err != nil {
				return err
			}
			redisCmd = listToRedisCmd(key, val)

		case "set":
			var val []string
			if err = client.Do(radix.Cmd(&val, "SMEMBERS", key)); err != nil {
				return err
			}
			redisCmd = setToRedisCmd(key, val)

		case "hash":
			var val map[string]string
			if err = client.Do(radix.Cmd(&val, "HGETALL", key)); err != nil {
				return err
			}
			redisCmd = hashToRedisCmd(key, val)

		case "zset":
			var val []string
			if err = client.Do(radix.Cmd(&val, "ZRANGEBYSCORE", key, "-inf", "+inf", "WITHSCORES")); err != nil {
				return err
			}
			redisCmd = zsetToRedisCmd(key, val)

		case "none":

		default:
			return fmt.Errorf("Key %s is of unreconized type %s", key, keyType)
		}

		logger.Print(serializer(redisCmd))

		if withTTL {
			var ttl int64
			if err = client.Do(radix.Cmd(&ttl, "TTL", key)); err != nil {
				return err
			}
			if ttl > 0 {
				redisCmd = ttlToRedisCmd(key, ttl)
				logger.Printf(serializer(redisCmd))
			}
		}
	}

	return nil
}

func dumpKeysWorker(client radix.Client, keyBatches <-chan []string, withTTL bool, logger *log.Logger, key_regexp string, serializer func([]string) string, errors chan<- error, done chan<- bool) {
	for keyBatch := range keyBatches {
		if err := dumpKeys(client, keyBatch, withTTL, logger, key_regexp, serializer); err != nil {
			errors <- err
		}
	}
	done <- true
}

// ProgressNotification message indicates the progress in dumping the Redis server,
// and can be used to provide a progress visualisation such as a progress bar.
// Done is the number of items dumped, Total is the total number of items to dump.
type ProgressNotification struct {
	Done, Total int
}

func parseKeyspaceInfo(keyspaceInfo string) ([]uint8, error) {
	var dbs []uint8

	scanner := bufio.NewScanner(strings.NewReader(keyspaceInfo))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.HasPrefix(line, "db") {
			continue
		}

		dbIndexString := line[2:strings.IndexAny(line, ":")]
		dbIndex, err := strconv.ParseUint(dbIndexString, 10, 8)
		if err != nil {
			return nil, err
		}
		if dbIndex > 16 {
			return nil, fmt.Errorf("Error parsing INFO keyspace")
		}

		dbs = append(dbs, uint8(dbIndex))
	}

	return dbs, nil
}

func getDBIndexes(redisURL, auth string) ([]uint8, error) {
	client, err := radix.NewPool("tcp", redisURL, 1, radix.PoolConnFunc(withDBSelection(radix.Dial, 0, auth)))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	var keyspaceInfo string
	if err = client.Do(radix.Cmd(&keyspaceInfo, "INFO", "keyspace")); err != nil {
		return nil, err
	}

	return parseKeyspaceInfo(keyspaceInfo)
}

func withDBSelection(dial radix.ConnFunc, db uint8, auth string) radix.ConnFunc {
	return func(network, addr string) (radix.Conn, error) {
		conn, err := dial(network, addr)
		if err != nil {
			return nil, err
		}

		if auth != "" {
			if err := conn.Do(radix.Cmd(nil, "AUTH", auth)); err != nil {
				conn.Close()
				return nil, err
			}
		}

		if err := conn.Do(radix.Cmd(nil, "SELECT", fmt.Sprint(db))); err != nil {
			conn.Close()
			return nil, err
		}

		return conn, nil
	}
}

func scanKeys(client radix.Client, keyBatches chan<- []string, progressNotifications chan<- ProgressNotification) error {
	keyBatchSize := 100
	s := radix.NewScanner(client, radix.ScanOpts{Command: "SCAN", Count: keyBatchSize})

	var dbSize int
	if err := client.Do(radix.Cmd(&dbSize, "DBSIZE")); err != nil {
		return err
	}

	nProcessed := 0
	var key string
	var keyBatch []string
	for s.Next(&key) {
		keyBatch = append(keyBatch, key)
		if len(keyBatch) >= keyBatchSize {
			nProcessed += len(keyBatch)
			keyBatches <- keyBatch
			keyBatch = nil
			progressNotifications <- ProgressNotification{nProcessed, dbSize}
		}
	}

	keyBatches <- keyBatch
	nProcessed += len(keyBatch)
	progressNotifications <- ProgressNotification{nProcessed, dbSize}

	return s.Close()
}

// DumpDB dumps all keys from a single Redis DB
func DumpDB(redisURL string, db uint8, nWorkers int, withTTL bool, logger *log.Logger, key_regexp string, auth string, serializer func([]string) string, progress chan<- ProgressNotification) error {
	var err error

	errors := make(chan error)
	nErrors := 0
	go func() {
		for err := range errors {
			fmt.Fprintln(os.Stderr, "Error: "+err.Error())
			nErrors++
		}
	}()

	client, err := radix.NewPool("tcp", redisURL, nWorkers, radix.PoolConnFunc(withDBSelection(radix.Dial, db, auth)))
	if err != nil {
		return err
	}
	defer client.Close()

	if err = client.Do(radix.Cmd(nil, "SELECT", fmt.Sprint(db))); err != nil {
		return err
	}
	logger.Printf(serializer([]string{"SELECT", fmt.Sprint(db)}))

	done := make(chan bool)
	keyBatches := make(chan []string)
	for i := 0; i < nWorkers; i++ {
		go dumpKeysWorker(client, keyBatches, withTTL, logger, key_regexp, serializer, errors, done)
	}

	scanKeys(client, keyBatches, progress)
	close(keyBatches)

	for i := 0; i < nWorkers; i++ {
		<-done
	}

	return nil
}

// DumpServer dumps all Keys from the redis server given by redisURL,
// to the Logger logger. Progress notification informations
// are regularly sent to the channel progressNotifications
func DumpServer(redisURL string, nWorkers int, withTTL bool, logger *log.Logger, key_regexp string, auth string, serializer func([]string) string, progress chan<- ProgressNotification) error {
	dbs, err := getDBIndexes(redisURL, auth)
	if err != nil {
		return err
	}

	for _, db := range dbs {
		if err = DumpDB(redisURL, db, nWorkers, withTTL, logger, key_regexp, auth, serializer, progress); err != nil {
			return err
		}
	}

	return nil
}

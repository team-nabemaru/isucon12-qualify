package isuports

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/pprof"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/rs/xid"
)

const (
	tenantDBSchemaFilePath = "../sql/tenant/10_schema.sql"
	initializeScript       = "../sql/init.sh"
	cookieName             = "isuports_session"

	RoleAdmin     = "admin"
	RoleOrganizer = "organizer"
	RolePlayer    = "player"
	RoleNone      = "none"
)

var (
	// 正しいテナント名の正規表現
	tenantNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

	adminDB     *sqlx.DB
	tenantDB    *sqlx.DB
	redisClient *RedisClient

	playerCache      *Cache[PlayerRow]      // ユーザー情報のキャッシュ
	competitionCache *Cache[CompetitionRow] // 大会情報のキャッシュ

	// sqliteDriverName = "sqlite3"
)

func playerCacheKey(playerId string) string {
	return fmt.Sprintf("player:%s", playerId)
}

func competitionCacheKey(competitonId string) string {
	return fmt.Sprintf("competition:%s", competitonId)
}

func latestplayerScoreCacheKey(competitonId string, playerId string) string {
	return fmt.Sprintf("player_score:%s-%s", competitonId, playerId)
}

func visitHistorySummaryCacheKey(competitonId string) string {
	return fmt.Sprintf("visit_history:%s", competitonId)
}

// 環境変数を取得する、なければデフォルト値を返す
func getEnv(key string, defaultValue string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultValue
}

// 管理用DBに接続する
func connectAdminDB() (*sqlx.DB, error) {
	config := mysql.NewConfig()
	config.Net = "tcp"
	config.Addr = getEnv("ISUCON_DB_HOST", "192.168.0.13") + ":" + getEnv("ISUCON_DB_PORT", "3306")
	config.User = getEnv("ISUCON_DB_USER", "isucon")
	config.Passwd = getEnv("ISUCON_DB_PASSWORD", "isucon")
	config.DBName = getEnv("ISUCON_DB_NAME", "isuports")
	config.ParseTime = true
	config.InterpolateParams = true
	dsn := config.FormatDSN()
	return sqlx.Open("mysql", dsn)
}

// テナントDBのパスを返す
// func tenantDBPath(id int64) string {
// 	tenantDBDir := getEnv("ISUCON_TENANT_DB_DIR", "../tenant_db")
// 	return filepath.Join(tenantDBDir, fmt.Sprintf("%d.db", id))
// }

// テナントDBに接続する
// func connectToTenantDB(id int64) (*sqlx.DB, error) {
// 	p := tenantDBPath(id)
// 	db, err := sqlx.Open(sqliteDriverName, fmt.Sprintf("file:%s?mode=rw", p))
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to open tenant DB: %w", err)
// 	}
// 	return db, nil
// }

// テナントDBを新規に作成する
// func createTenantDB(id int64) error {
// 	p := tenantDBPath(id)

// 	cmd := exec.Command("sh", "-c", fmt.Sprintf("sqlite3 %s < %s", p, tenantDBSchemaFilePath))
// 	if out, err := cmd.CombinedOutput(); err != nil {
// 		return fmt.Errorf("failed to exec sqlite3 %s < %s, out=%s: %w", p, tenantDBSchemaFilePath, string(out), err)
// 	}
// 	return nil
// }

// システム全体で一意なIDを生成する
func dispenseID(ctx context.Context) (string, error) {
	return xid.New().String(), nil
}

// 全APIにCache-Control: privateを設定する
func SetCacheControlPrivate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderCacheControl, "private")
		return next(c)
	}
}

// Run は cmd/isuports/main.go から呼ばれるエントリーポイントです
func Run() {
	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(log.DEBUG)
	pprof.Register(e)

	var (
		// sqlLogger io.Closer
		err error
	)
	// sqliteのクエリログを出力する設定
	// 環境変数 ISUCON_SQLITE_TRACE_FILE を設定すると、そのファイルにクエリログをJSON形式で出力する
	// 未設定なら出力しない
	// sqltrace.go を参照
	// sqliteDriverName, sqlLogger, err = initializeSQLLogger()
	// if err != nil {
	// 	e.Logger.Panicf("error initializeSQLLogger: %s", err)
	// }
	// defer sqlLogger.Close()

	redisClient = NewRedisClient(context.TODO())
	playerCache = NewCache[PlayerRow](*redisClient, 60*time.Second)
	competitionCache = NewCache[CompetitionRow](*redisClient, 60*time.Second)

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(SetCacheControlPrivate)

	// SaaS管理者向けAPI
	e.POST("/api/admin/tenants/add", tenantsAddHandler)
	e.GET("/api/admin/tenants/billing", tenantsBillingHandler)

	// テナント管理者向けAPI - 参加者追加、一覧、失格
	e.GET("/api/organizer/players", playersListHandler)
	e.POST("/api/organizer/players/add", playersAddHandler)
	e.POST("/api/organizer/player/:player_id/disqualified", playerDisqualifiedHandler)

	// テナント管理者向けAPI - 大会管理
	e.POST("/api/organizer/competitions/add", competitionsAddHandler)
	e.POST("/api/organizer/competition/:competition_id/finish", competitionFinishHandler)
	e.POST("/api/organizer/competition/:competition_id/score", competitionScoreHandler)
	e.GET("/api/organizer/billing", billingHandler)
	e.GET("/api/organizer/competitions", organizerCompetitionsHandler)

	// 参加者向けAPI
	e.GET("/api/player/player/:player_id", playerHandler)
	e.GET("/api/player/competition/:competition_id/ranking", competitionRankingHandler)
	e.GET("/api/player/competitions", playerCompetitionsHandler)

	// 全ロール及び未認証でも使えるhandler
	e.GET("/api/me", meHandler)

	// ベンチマーカー向けAPI
	e.POST("/initialize", initializeHandler)

	e.HTTPErrorHandler = errorResponseHandler

	adminDB, err = connectAdminDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	adminDB.SetMaxOpenConns(30)

	tenantDB = adminDB

	// hostname, _ := os.Hostname()
	// if hostname == "ip-192-168-0-12" {
	// 	go func() {
	// 		for {
	// 			ctx := context.TODO()
	// 			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)

	// 			playerScores := []PlayerScoreRow{}
	// 			if err := tenantDB.SelectContext(ctx, &playerScores,
	// 				"SELECT `player_id`, `competition_id`, `score`, MAX(row_num) AS row_num FROM player_score GROUP BY tenant_id, competition_id, player_id, score"); err != nil {
	// 				fmt.Println("error Select player_score: ", err)
	// 			}

	// 			playerScoresMap := map[string][]PlayerScoreRow{}
	// 			for _, playerScore := range playerScores {
	// 				key := playerScoreCacheKey(playerScore.CompetitionID)

	// 				if _, ok := playerScoresMap[key]; !ok {
	// 					playerScoresMap[key] = []PlayerScoreRow{}
	// 				}

	// 				playerScoresMap[key] = append(playerScoresMap[key], playerScore)
	// 			}

	// 			for key, playerScores := range playerScoresMap {
	// 				bytes, err := json.Marshal(playerScores)
	// 				if err != nil {
	// 					fmt.Println("error Marshal player_score: ", err)
	// 					continue
	// 				}

	// 				if err := redisClient.client.Set(ctx, key, bytes, 10*time.Second).Err(); err != nil {
	// 					fmt.Println("error Set player_score: ", err)
	// 					continue
	// 				}
	// 			}

	// 			cancel()
	// 			// time.Sleep(3 * time.Second)
	// 		}
	// 	}()
	// }

	defer adminDB.Close()

	port := getEnv("SERVER_APP_PORT", "3000")
	e.Logger.Infof("starting isuports server on : %s ...", port)
	serverPort := fmt.Sprintf(":%s", port)
	e.Logger.Fatal(e.Start(serverPort))
}

// エラー処理関数
func errorResponseHandler(err error, c echo.Context) {
	c.Logger().Errorf("error at %s: %s", c.Path(), err.Error())
	var he *echo.HTTPError
	if errors.As(err, &he) {
		c.JSON(he.Code, FailureResult{
			Status: false,
		})
		return
	}
	c.JSON(http.StatusInternalServerError, FailureResult{
		Status: false,
	})
}

type SuccessResult struct {
	Status bool `json:"status"`
	Data   any  `json:"data,omitempty"`
}

type FailureResult struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
}

// アクセスしてきた人の情報
type Viewer struct {
	role       string
	playerID   string
	tenantName string
	tenantID   int64
}

// リクエストヘッダをパースしてViewerを返す
func parseViewer(c echo.Context) (*Viewer, error) {
	cookie, err := c.Request().Cookie(cookieName)
	if err != nil {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("cookie %s is not found", cookieName),
		)
	}
	tokenStr := cookie.Value

	keyFilename := getEnv("ISUCON_JWT_KEY_FILE", "../public.pem")
	keysrc, err := os.ReadFile(keyFilename)
	if err != nil {
		return nil, fmt.Errorf("error os.ReadFile: keyFilename=%s: %w", keyFilename, err)
	}
	key, _, err := jwk.DecodePEM(keysrc)
	if err != nil {
		return nil, fmt.Errorf("error jwk.DecodePEM: %w", err)
	}

	token, err := jwt.Parse(
		[]byte(tokenStr),
		jwt.WithKey(jwa.RS256, key),
	)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, fmt.Errorf("error jwt.Parse: %s", err.Error()))
	}
	if token.Subject() == "" {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: subject is not found in token: %s", tokenStr),
		)
	}

	var role string
	tr, ok := token.Get("role")
	if !ok {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: role is not found: %s", tokenStr),
		)
	}
	switch tr {
	case RoleAdmin, RoleOrganizer, RolePlayer:
		role = tr.(string)
	default:
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: invalid role: %s", tokenStr),
		)
	}
	// aud は1要素でテナント名がはいっている
	aud := token.Audience()
	if len(aud) != 1 {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: aud field is few or too much: %s", tokenStr),
		)
	}
	tenant, err := retrieveTenantRowFromHeader(c)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
		}
		return nil, fmt.Errorf("error retrieveTenantRowFromHeader at parseViewer: %w", err)
	}
	if tenant.Name == "admin" && role != RoleAdmin {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
	}

	if tenant.Name != aud[0] {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: tenant name is not match with %s: %s", c.Request().Host, tokenStr),
		)
	}

	v := &Viewer{
		role:       role,
		playerID:   token.Subject(),
		tenantName: tenant.Name,
		tenantID:   tenant.ID,
	}
	return v, nil
}

func retrieveTenantRowFromHeader(c echo.Context) (*TenantRow, error) {
	// JWTに入っているテナント名とHostヘッダのテナント名が一致しているか確認
	baseHost := getEnv("ISUCON_BASE_HOSTNAME", ".t.isucon.dev")
	tenantName := strings.TrimSuffix(c.Request().Host, baseHost)

	// SaaS管理者用ドメイン
	if tenantName == "admin" {
		return &TenantRow{
			Name:        "admin",
			DisplayName: "admin",
		}, nil
	}

	// テナントの存在確認
	var tenant TenantRow
	if err := adminDB.GetContext(
		context.Background(),
		&tenant,
		"SELECT * FROM tenant WHERE name = ?",
		tenantName,
	); err != nil {
		return nil, fmt.Errorf("failed to Select tenant: name=%s, %w", tenantName, err)
	}
	return &tenant, nil
}

type TenantRow struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	DisplayName string `db:"display_name"`
	CreatedAt   int64  `db:"created_at"`
	UpdatedAt   int64  `db:"updated_at"`
}

type dbOrTx interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

type PlayerRow struct {
	TenantID       int64  `db:"tenant_id"`
	ID             string `db:"id"`
	DisplayName    string `db:"display_name"`
	IsDisqualified bool   `db:"is_disqualified"`
	CreatedAt      int64  `db:"created_at"`
	UpdatedAt      int64  `db:"updated_at"`
}

// 参加者を取得する
func retrievePlayer(ctx context.Context, tenantDB dbOrTx, id string) (*PlayerRow, error) {
	key := playerCacheKey(id)
	p, err := playerCache.GetOrSet(ctx, key, func(ctx context.Context) (PlayerRow, error) {
		var p PlayerRow
		if err := tenantDB.GetContext(ctx, &p, "SELECT * FROM player WHERE id = ?", id); err != nil {
			return PlayerRow{}, fmt.Errorf("error Select player: id=%s, %w", id, err)
		}
		return p, nil
	})
	return &p, err
}

// func retrievePlayerScores(ctx context.Context, tenantDB dbOrTx, competitonID string) ([]PlayerScoreRow, error) {
// 	key := playerScoreCacheKey(competitonID)
// 	bytes, result, _ := redisClient.Get(ctx, key)
// 	if !result {
// 		fmt.Println("Miss cache", competitonID)
// 		var playerScores []PlayerScoreRow
// 		if err := tenantDB.SelectContext(
// 			ctx,
// 			&playerScores,
// 			"SELECT `player_id`, `competition_id`, `score`, MAX(row_num) AS row_num FROM player_score WHERE competition_id = ? GROUP BY tenant_id, competition_id, player_id, score",
// 			competitonID,
// 		); err != nil {
// 			return nil, fmt.Errorf("error Select player_score: %w", err)
// 		}

// 		bytes, err := json.Marshal(playerScores)
// 		if err == nil {
// 			redisClient.client.Set(ctx, key, bytes, 10*time.Second)
// 		}

// 		return playerScores, nil
// 	}
// 	fmt.Println("Hit cache", competitonID)

// 	var playerScores []PlayerScoreRow
// 	if err := json.Unmarshal(bytes, &playerScores); err != nil {
// 		return nil, err
// 	}

// 	return playerScores, nil
// }

func retrieveVisitHistorySummaryRow(ctx context.Context, tenantDB dbOrTx, competitonID string) ([]VisitHistorySummaryRow, error) {
	key := visitHistorySummaryCacheKey(competitonID)
	bytes, result, _ := redisClient.Get(ctx, key)
	if !result {
		var vhs []VisitHistorySummaryRow
		if err := tenantDB.SelectContext(
			ctx,
			&vhs,
			"SELECT player_id, created_at AS min_created_at FROM visit_history WHERE competition_id = ?",
			competitonID,
		); err != nil {
			return nil, fmt.Errorf("error Select visit_history: %w", err)
		}

		bytes, err := json.Marshal(vhs)
		if err == nil {
			redisClient.client.Set(ctx, key, bytes, 10*time.Second)
		}

		return vhs, nil
	}

	var vhs []VisitHistorySummaryRow
	if err := json.Unmarshal(bytes, &vhs); err != nil {
		return nil, err
	}

	return vhs, nil
}

// 参加者を認可する
// 参加者向けAPIで呼ばれる
func authorizePlayer(ctx context.Context, tenantDB dbOrTx, id string) error {
	player, err := retrievePlayer(ctx, tenantDB, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusUnauthorized, "player not found")
		}
		return fmt.Errorf("error retrievePlayer from viewer: %w", err)
	}
	if player.IsDisqualified {
		return echo.NewHTTPError(http.StatusForbidden, "player is disqualified")
	}
	return nil
}

type CompetitionRow struct {
	TenantID   int64         `db:"tenant_id"`
	ID         string        `db:"id"`
	Title      string        `db:"title"`
	FinishedAt sql.NullInt64 `db:"finished_at"`
	CreatedAt  int64         `db:"created_at"`
	UpdatedAt  int64         `db:"updated_at"`
}

// 大会を取得する
func retrieveCompetition(ctx context.Context, tenantDB dbOrTx, id string) (*CompetitionRow, error) {
	key := competitionCacheKey(id)
	c, err := competitionCache.GetOrSet(ctx, key, func(ctx context.Context) (CompetitionRow, error) {
		var c CompetitionRow
		if err := tenantDB.GetContext(ctx, &c, "SELECT * FROM competition WHERE id = ?", id); err != nil {
			return CompetitionRow{}, fmt.Errorf("error Select competition: id=%s, %w", id, err)
		}
		return c, nil
	})
	return &c, err
}

type PlayerScoreRow struct {
	TenantID      int64  `db:"tenant_id"`
	ID            string `db:"id"`
	PlayerID      string `db:"player_id"`
	CompetitionID string `db:"competition_id"`
	Score         int64  `db:"score"`
	RowNum        int64  `db:"row_num"`
	CreatedAt     int64  `db:"created_at"`
	UpdatedAt     int64  `db:"updated_at"`
}

// 排他ロックのためのファイル名を生成する
// func lockFilePath(id int64) string {
// 	tenantDBDir := getEnv("ISUCON_TENANT_DB_DIR", "../tenant_db")
// 	return filepath.Join(tenantDBDir, fmt.Sprintf("%d.lock", id))
// }

// 排他ロックする
// func flockByTenantID(tenantID int64) (io.Closer, error) {
// 	p := lockFilePath(tenantID)

// 	fl := flock.New(p)
// 	if err := fl.Lock(); err != nil {
// 		return nil, fmt.Errorf("error flock.Lock: path=%s, %w", p, err)
// 	}
// 	return fl, nil
// }

type TenantsAddHandlerResult struct {
	Tenant TenantWithBilling `json:"tenant"`
}

// SasS管理者用API
// テナントを追加する
// POST /api/admin/tenants/add
func tenantsAddHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.tenantName != "admin" {
		// admin: SaaS管理者用の特別なテナント名
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("%s has not this API", v.tenantName),
		)
	}
	if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	displayName := c.FormValue("display_name")
	name := c.FormValue("name")
	if err := validateTenantName(name); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	now := time.Now().Unix()
	insertRes, err := adminDB.ExecContext(
		c.Request().Context(),
		"INSERT INTO tenant (name, display_name, created_at, updated_at) VALUES (?, ?, ?, ?)",
		name, displayName, now, now,
	)
	if err != nil {
		if merr, ok := err.(*mysql.MySQLError); ok && merr.Number == 1062 { // duplicate entry
			return echo.NewHTTPError(http.StatusBadRequest, "duplicate tenant")
		}
		return fmt.Errorf(
			"error Insert tenant: name=%s, displayName=%s, createdAt=%d, updatedAt=%d, %w",
			name, displayName, now, now, err,
		)
	}

	id, err := insertRes.LastInsertId()
	if err != nil {
		return fmt.Errorf("error get LastInsertId: %w", err)
	}
	// NOTE: 先にadminDBに書き込まれることでこのAPIの処理中に
	//       /api/admin/tenants/billingにアクセスされるとエラーになりそう
	//       ロックなどで対処したほうが良さそう
	// if err := createTenantDB(id); err != nil {
	// 	return fmt.Errorf("error createTenantDB: id=%d name=%s %w", id, name, err)
	// }

	res := TenantsAddHandlerResult{
		Tenant: TenantWithBilling{
			ID:          strconv.FormatInt(id, 10),
			Name:        name,
			DisplayName: displayName,
			BillingYen:  0,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

// テナント名が規則に沿っているかチェックする
func validateTenantName(name string) error {
	if tenantNameRegexp.MatchString(name) {
		return nil
	}
	return fmt.Errorf("invalid tenant name: %s", name)
}

type BillingReport struct {
	CompetitionID     string `json:"competition_id"`
	CompetitionTitle  string `json:"competition_title"`
	PlayerCount       int64  `json:"player_count"`        // スコアを登録した参加者数
	VisitorCount      int64  `json:"visitor_count"`       // ランキングを閲覧だけした(スコアを登録していない)参加者数
	BillingPlayerYen  int64  `json:"billing_player_yen"`  // 請求金額 スコアを登録した参加者分
	BillingVisitorYen int64  `json:"billing_visitor_yen"` // 請求金額 ランキングを閲覧だけした(スコアを登録していない)参加者分
	BillingYen        int64  `json:"billing_yen"`         // 合計請求金額
}

type VisitHistoryRow struct {
	PlayerID      string `db:"player_id"`
	TenantID      int64  `db:"tenant_id"`
	CompetitionID string `db:"competition_id"`
	CreatedAt     int64  `db:"created_at"`
	UpdatedAt     int64  `db:"updated_at"`
}

type VisitHistorySummaryRow struct {
	PlayerID     string `db:"player_id"`
	MinCreatedAt int64  `db:"min_created_at"`
}

// 大会ごとの課金レポートを計算する
func billingReportByCompetition(ctx context.Context, tenantDB dbOrTx, tenantID int64, competitonID string) (*BillingReport, error) {
	comp, err := retrieveCompetition(ctx, tenantDB, competitonID)
	if err != nil {
		return nil, fmt.Errorf("error retrieveCompetition: %w", err)
	}

	// ランキングにアクセスした参加者のIDを取得する
	// vhs := []VisitHistorySummaryRow{}
	// if err := adminDB.SelectContext(
	// 	ctx,
	// 	&vhs,
	// 	"SELECT player_id, created_at AS min_created_at FROM visit_history WHERE competition_id = ?",
	// 	comp.ID,
	// ); err != nil && err != sql.ErrNoRows {
	// 	return nil, fmt.Errorf("error Select visit_history: tenantID=%d, competitionID=%s, %w", tenantID, comp.ID, err)
	// }
	vhs, err := retrieveVisitHistorySummaryRow(ctx, tenantDB, competitonID)
	if err != nil {
		return nil, fmt.Errorf("error retrieveVisitHistorySummaryRow: %w", err)
	}

	billingMap := map[string]string{}
	for _, vh := range vhs {
		// competition.finished_atよりもあとの場合は、終了後に訪問したとみなして大会開催内アクセス済みとみなさない
		if comp.FinishedAt.Valid && comp.FinishedAt.Int64 < vh.MinCreatedAt {
			continue
		}
		billingMap[vh.PlayerID] = "visitor"
	}

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	// fl, err := flockByTenantID(tenantID)
	// if err != nil {
	// 	return nil, fmt.Errorf("error flockByTenantID: %w", err)
	// }
	// defer fl.Close()

	// スコアを登録した参加者のIDを取得する
	scoredPlayerIDs := []string{}
	if err := tenantDB.SelectContext(
		ctx,
		&scoredPlayerIDs,
		// "SELECT DISTINCT(player_id) FROM player_score WHERE tenant_id = ? AND competition_id = ?",
		"SELECT player_id FROM latest_player_score WHERE competition_id = ?",
		comp.ID,
	); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("error Select count player_score: tenantID=%d, competitionID=%s, %w", tenantID, competitonID, err)
	}
	for _, pid := range scoredPlayerIDs {
		// スコアが登録されている参加者
		billingMap[pid] = "player"
	}

	// 大会が終了している場合のみ請求金額が確定するので計算する
	var playerCount, visitorCount int64
	if comp.FinishedAt.Valid {
		for _, category := range billingMap {
			switch category {
			case "player":
				playerCount++
			case "visitor":
				visitorCount++
			}
		}
	}
	return &BillingReport{
		CompetitionID:     comp.ID,
		CompetitionTitle:  comp.Title,
		PlayerCount:       playerCount,
		VisitorCount:      visitorCount,
		BillingPlayerYen:  100 * playerCount, // スコアを登録した参加者は100円
		BillingVisitorYen: 10 * visitorCount, // ランキングを閲覧だけした(スコアを登録していない)参加者は10円
		BillingYen:        100*playerCount + 10*visitorCount,
	}, nil
}

type TenantWithBilling struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	BillingYen  int64  `json:"billing"`
}

type TenantsBillingHandlerResult struct {
	Tenants []TenantWithBilling `json:"tenants"`
}

// SaaS管理者用API
// テナントごとの課金レポートを最大10件、テナントのid降順で取得する
// GET /api/admin/tenants/billing
// URL引数beforeを指定した場合、指定した値よりもidが小さいテナントの課金レポートを取得する
func tenantsBillingHandler(c echo.Context) error {
	if host := c.Request().Host; host != getEnv("ISUCON_ADMIN_HOSTNAME", "admin.t.isucon.dev") {
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("invalid hostname %s", host),
		)
	}

	ctx := context.Background()
	if v, err := parseViewer(c); err != nil {
		return err
	} else if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	before := c.QueryParam("before")
	var beforeID int64
	if before != "" {
		var err error
		beforeID, err = strconv.ParseInt(before, 10, 64)
		if err != nil {
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("failed to parse query parameter 'before': %s", err.Error()),
			)
		}
	}
	// テナントごとに
	//   大会ごとに
	//     scoreが登録されているplayer * 100
	//     scoreが登録されていないplayerでアクセスした人 * 10
	//   を合計したものを
	// テナントの課金とする
	ts := []TenantRow{}
	if err := adminDB.SelectContext(ctx, &ts, "SELECT * FROM tenant ORDER BY id DESC"); err != nil {
		return fmt.Errorf("error Select tenant: %w", err)
	}
	tenantBillings := make([]TenantWithBilling, 0, len(ts))
	for _, t := range ts {
		if beforeID != 0 && beforeID <= t.ID {
			continue
		}
		err := func(t TenantRow) error {
			tb := TenantWithBilling{
				ID:          strconv.FormatInt(t.ID, 10),
				Name:        t.Name,
				DisplayName: t.DisplayName,
			}
			cs := []CompetitionRow{}
			if err := tenantDB.SelectContext(
				ctx,
				&cs,
				"SELECT * FROM competition WHERE tenant_id=?",
				t.ID,
			); err != nil {
				return fmt.Errorf("failed to Select competition: %w", err)
			}
			for _, comp := range cs {
				report, err := billingReportByCompetition(ctx, tenantDB, t.ID, comp.ID)
				if err != nil {
					return fmt.Errorf("failed to billingReportByCompetition: %w", err)
				}
				tb.BillingYen += report.BillingYen
			}
			tenantBillings = append(tenantBillings, tb)
			return nil
		}(t)
		if err != nil {
			return err
		}
		if len(tenantBillings) >= 10 {
			break
		}
	}
	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data: TenantsBillingHandlerResult{
			Tenants: tenantBillings,
		},
	})
}

type PlayerDetail struct {
	ID             string `json:"id" db:"id"`
	DisplayName    string `json:"display_name" db:"display_name"`
	IsDisqualified bool   `json:"is_disqualified" db:"is_disqualified"`
}

type PlayersListHandlerResult struct {
	Players []PlayerDetail `json:"players"`
}

// テナント管理者向けAPI
// GET /api/organizer/players
// 参加者一覧を返す
func playersListHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return err
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	var pls []PlayerRow
	if err := tenantDB.SelectContext(
		ctx,
		&pls,
		"SELECT * FROM player WHERE tenant_id=? ORDER BY created_at DESC",
		v.tenantID,
	); err != nil {
		return fmt.Errorf("error Select player: %w", err)
	}
	var pds []PlayerDetail
	for _, p := range pls {
		pds = append(pds, PlayerDetail{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			IsDisqualified: p.IsDisqualified,
		})
	}

	res := PlayersListHandlerResult{
		Players: pds,
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type PlayersAddHandlerResult struct {
	Players []PlayerDetail `json:"players"`
}

// テナント管理者向けAPI
// GET /api/organizer/players/add
// テナントに参加者を追加する
func playersAddHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	params, err := c.FormParams()
	if err != nil {
		return fmt.Errorf("error c.FormParams: %w", err)
	}
	displayNames := params["display_name[]"]

	stmt := "INSERT INTO player (id, tenant_id, display_name, is_disqualified, created_at, updated_at) VALUES "
	now := time.Now().Unix()
	id_array := make([]string, 0, len(displayNames))
	for _, displayName := range displayNames {
		id, err := dispenseID(ctx)
		if err != nil {
			return fmt.Errorf("error dispenseID: %w", err)
		}
		stmt += fmt.Sprintf(
			"('%s', %d, '%s', %t, %d, %d),",
			id, v.tenantID, displayName, false, now, now,
		)
		id_array = append(id_array, fmt.Sprintf("'%s'", id))
	}
	stmt = stmt[:len(stmt)-1]
	_, err = tenantDB.Exec(stmt)
	if err != nil {
		return fmt.Errorf("error retrievePlayer: %w", err)
	}

	pds := make([]PlayerDetail, 0, len(displayNames))

	stmt2 := "SELECT `id`, `display_name`, `is_disqualified` FROM player" + `
					  WHERE id IN (` + strings.Join(id_array, ",") + `)
	`

	if err := tenantDB.SelectContext(
		ctx,
		&pds,
		stmt2,
	); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("error retrievePlayer: %w", err)
	}

	res := PlayersAddHandlerResult{
		Players: pds,
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type PlayerDisqualifiedHandlerResult struct {
	Player PlayerDetail `json:"player"`
}

// テナント管理者向けAPI
// POST /api/organizer/player/:player_id/disqualified
// 参加者を失格にする
func playerDisqualifiedHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	playerID := c.Param("player_id")

	now := time.Now().Unix()
	if _, err := tenantDB.ExecContext(
		ctx,
		"UPDATE player SET is_disqualified = ?, updated_at = ? WHERE id = ?",
		true, now, playerID,
	); err != nil {
		return fmt.Errorf(
			"error Update player: isDisqualified=%t, updatedAt=%d, id=%s, %w",
			true, now, playerID, err,
		)
	}

	if err := playerCache.client.Del(ctx, playerCacheKey(playerID)); err != nil {
		return fmt.Errorf("error Redis Del: %w", err)
	}

	p, err := retrievePlayer(ctx, tenantDB, playerID)
	if err != nil {
		// 存在しないプレイヤー
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "player not found")
		}
		return fmt.Errorf("error retrievePlayer: %w", err)
	}

	res := PlayerDisqualifiedHandlerResult{
		Player: PlayerDetail{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			IsDisqualified: p.IsDisqualified,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type CompetitionDetail struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	IsFinished bool   `json:"is_finished"`
}

type CompetitionsAddHandlerResult struct {
	Competition CompetitionDetail `json:"competition"`
}

// テナント管理者向けAPI
// POST /api/organizer/competitions/add
// 大会を追加する
func competitionsAddHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	title := c.FormValue("title")

	now := time.Now().Unix()
	id, err := dispenseID(ctx)
	if err != nil {
		return fmt.Errorf("error dispenseID: %w", err)
	}
	if _, err := tenantDB.ExecContext(
		ctx,
		"INSERT INTO competition (id, tenant_id, title, finished_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, v.tenantID, title, sql.NullInt64{}, now, now,
	); err != nil {
		return fmt.Errorf(
			"error Insert competition: id=%s, tenant_id=%d, title=%s, finishedAt=null, createdAt=%d, updatedAt=%d, %w",
			id, v.tenantID, title, now, now, err,
		)
	}

	res := CompetitionsAddHandlerResult{
		Competition: CompetitionDetail{
			ID:         id,
			Title:      title,
			IsFinished: false,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

// テナント管理者向けAPI
// POST /api/organizer/competition/:competition_id/finish
// 大会を終了する
func competitionFinishHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	id := c.Param("competition_id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id required")
	}
	_, err = retrieveCompetition(ctx, tenantDB, id)
	if err != nil {
		// 存在しない大会
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "competition not found")
		}
		return fmt.Errorf("error retrieveCompetition: %w", err)
	}

	now := time.Now().Unix()
	if _, err := tenantDB.ExecContext(
		ctx,
		"UPDATE competition SET finished_at = ?, updated_at = ? WHERE id = ?",
		now, now, id,
	); err != nil {
		return fmt.Errorf(
			"error Update competition: finishedAt=%d, updatedAt=%d, id=%s, %w",
			now, now, id, err,
		)
	}

	if err := redisClient.client.Del(ctx, competitionCacheKey(id)).Err(); err != nil {
		return fmt.Errorf("error Redis Del: %w", err)
	}

	return c.JSON(http.StatusOK, SuccessResult{Status: true})
}

type ScoreHandlerResult struct {
	Rows int64 `json:"rows"`
}

// テナント管理者向けAPI
// POST /api/organizer/competition/:competition_id/score
// 大会のスコアをCSVでアップロードする
func competitionScoreHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	competitionID := c.Param("competition_id")
	if competitionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id required")
	}
	comp, err := retrieveCompetition(ctx, tenantDB, competitionID)
	if err != nil {
		// 存在しない大会
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "competition not found")
		}
		return fmt.Errorf("error retrieveCompetition: %w", err)
	}
	if comp.FinishedAt.Valid {
		res := FailureResult{
			Status:  false,
			Message: "competition is finished",
		}
		return c.JSON(http.StatusBadRequest, res)
	}

	fh, err := c.FormFile("scores")
	if err != nil {
		return fmt.Errorf("error c.FormFile(scores): %w", err)
	}
	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("error fh.Open FormFile(scores): %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	headers, err := r.Read()
	if err != nil {
		return fmt.Errorf("error r.Read at header: %w", err)
	}
	if !reflect.DeepEqual(headers, []string{"player_id", "score"}) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid CSV headers")
	}

	var rowNum int64
	playerScoreRows := []PlayerScoreRow{}
	for {
		rowNum++
		row, err := r.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error r.Read at rows: %w", err)
		}
		if len(row) != 2 {
			return fmt.Errorf("row must have two columns: %#v", row)
		}
		playerID, scoreStr := row[0], row[1]
		if _, err := retrievePlayer(ctx, tenantDB, playerID); err != nil {
			// 存在しない参加者が含まれている
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(
					http.StatusBadRequest,
					fmt.Sprintf("player not found: %s", playerID),
				)
			}
			return fmt.Errorf("error retrievePlayer: %w", err)
		}
		var score int64
		if score, err = strconv.ParseInt(scoreStr, 10, 64); err != nil {
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("error strconv.ParseUint: scoreStr=%s, %s", scoreStr, err),
			)
		}
		id, err := dispenseID(ctx)
		if err != nil {
			return fmt.Errorf("error dispenseID: %w", err)
		}
		now := time.Now().Unix()
		playerScoreRows = append(playerScoreRows, PlayerScoreRow{
			ID:            id,
			TenantID:      v.tenantID,
			PlayerID:      playerID,
			CompetitionID: competitionID,
			Score:         score,
			RowNum:        rowNum,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}

	tx, err := tenantDB.Beginx()
	if err != nil {
		return fmt.Errorf("error tenantDB.Beginx: %w", err)
	}
	defer tx.Rollback()

	deleted := false
	if err := tx.GetContext(
		ctx,
		nil,
		"SELECT count(1) FROM player_score WHERE competition_id = ? limit 1",
		competitionID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			deleted = true
		}
	}

	if !deleted {
		if _, err := tx.ExecContext(
			ctx,
			"DELETE FROM player_score WHERE competition_id = ?",
			competitionID,
		); err != nil {
			return fmt.Errorf("error Delete player_score: tenantID=%d, competitionID=%s, %w", v.tenantID, competitionID, err)
		}
	}

	sql := "INSERT INTO player_score (id, tenant_id, player_id, competition_id, score, row_num, created_at, updated_at) VALUES "
	for _, ps := range playerScoreRows {
		sql += fmt.Sprintf(
			"('%s', %d, '%s', '%s', %d, %d, %d, %d),",
			ps.ID, ps.TenantID, ps.PlayerID, ps.CompetitionID, ps.Score, ps.RowNum, ps.CreatedAt, ps.UpdatedAt,
		)

		// if _, err := tx.NamedExecContext(
		// 	ctx,
		// 	"INSERT INTO player_score (id, tenant_id, player_id, competition_id, score, row_num, created_at, updated_at) VALUES (:id, :tenant_id, :player_id, :competition_id, :score, :row_num, :created_at, :updated_at)",
		// 	ps,
		// ); err != nil {
		// 	return fmt.Errorf(
		// 		"error Insert player_score: id=%s, tenant_id=%d, playerID=%s, competitionID=%s, score=%d, rowNum=%d, createdAt=%d, updatedAt=%d, %w",
		// 		ps.ID, ps.TenantID, ps.PlayerID, ps.CompetitionID, ps.Score, ps.RowNum, ps.CreatedAt, ps.UpdatedAt, err,
		// 	)
		// }
	}
	sql = sql[:len(sql)-1]

	retry := 5
	for {
		_, err = tx.Exec(sql)
		if err != nil {
			if retry > 0 {
				retry--
				continue
			}
			// c.Logger().Errorf("db error: %v", err)
			// return c.NoContent(http.StatusInternalServerError)
			return fmt.Errorf("error Insert player_score: %w", err)
		}
		break
	}

	tx.Commit()

	// go func() {
	// 	key := playerScoreCacheKey(competitionID)
	// 	redisClient.client.Del(ctx, key)
	// }()

	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data:   ScoreHandlerResult{Rows: int64(len(playerScoreRows))},
	})
}

type BillingHandlerResult struct {
	Reports []BillingReport `json:"reports"`
}

// テナント管理者向けAPI
// GET /api/organizer/billing
// テナント内の課金レポートを取得する
func billingHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	cs := []CompetitionRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&cs,
		"SELECT * FROM competition WHERE tenant_id=? ORDER BY created_at DESC",
		v.tenantID,
	); err != nil {
		return fmt.Errorf("error Select competition: %w", err)
	}
	tbrs := make([]BillingReport, 0, len(cs))
	for _, comp := range cs {
		report, err := billingReportByCompetition(ctx, tenantDB, v.tenantID, comp.ID)
		if err != nil {
			return fmt.Errorf("error billingReportByCompetition: %w", err)
		}
		tbrs = append(tbrs, *report)
	}

	res := SuccessResult{
		Status: true,
		Data: BillingHandlerResult{
			Reports: tbrs,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type PlayerScoreDetail struct {
	CompetitionTitle string `json:"competition_title"`
	Score            int64  `json:"score"`
}

type PlayerHandlerResult struct {
	Player PlayerDetail        `json:"player"`
	Scores []PlayerScoreDetail `json:"scores"`
}

// 参加者向けAPI
// GET /api/player/player/:player_id
// 参加者の詳細情報を取得する
func playerHandler(c echo.Context) error {
	ctx := context.Background()

	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}

	playerID := c.Param("player_id")
	if playerID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "player_id is required")
	}

	p, err := retrievePlayer(ctx, tenantDB, playerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "player not found")
		}
		return fmt.Errorf("error retrievePlayer: %w", err)
	}
	cs := []CompetitionRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&cs,
		"SELECT `id` FROM competition WHERE tenant_id = ? ORDER BY created_at ASC",
		v.tenantID,
	); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("error Select competition: %w", err)
	}

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	// fl, err := flockByTenantID(v.tenantID)
	// if err != nil {
	// 	return fmt.Errorf("error flockByTenantID: %w", err)
	// }
	// defer fl.Close()
	pss := make([]PlayerScoreRow, 0, len(cs))

	cs_id_array := make([]string, 0, len(cs))
	for _, c := range cs {
		cs_id_array = append(cs_id_array, fmt.Sprintf("'%s'", c.ID))
	}
	stmt := "SELECT `competition_id`, `player_id`, `score`" + `
					 FROM latest_player_score
					 WHERE player_id = ?
					 AND competition_id IN (` + strings.Join(cs_id_array, ",") + `)
	`

	if err := tenantDB.SelectContext(
		ctx,
		&pss,
		stmt,
		p.ID,
	); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("error Select latest_player_score: %w", err)
	}

	psds := make([]PlayerScoreDetail, 0, len(pss))
	for _, ps := range pss {
		comp, err := retrieveCompetition(ctx, tenantDB, ps.CompetitionID)
		if err != nil {
			return fmt.Errorf("error retrieveCompetition: %w", err)
		}
		psds = append(psds, PlayerScoreDetail{
			CompetitionTitle: comp.Title,
			Score:            ps.Score,
		})
	}

	res := SuccessResult{
		Status: true,
		Data: PlayerHandlerResult{
			Player: PlayerDetail{
				ID:             p.ID,
				DisplayName:    p.DisplayName,
				IsDisqualified: p.IsDisqualified,
			},
			Scores: psds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type CompetitionRank struct {
	Rank              int64  `json:"rank"`
	Score             int64  `json:"score"`
	PlayerID          string `json:"player_id"`
	PlayerDisplayName string `json:"player_display_name"`
	RowNum            int64  `json:"-"` // APIレスポンスのJSONには含まれない
}

type CompetitionRankingHandlerResult struct {
	Competition CompetitionDetail `json:"competition"`
	Ranks       []CompetitionRank `json:"ranks"`
}

// 参加者向けAPI
// GET /api/player/competition/:competition_id/ranking
// 大会ごとのランキングを取得する
func competitionRankingHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}

	competitionID := c.Param("competition_id")
	if competitionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id is required")
	}

	// 大会の存在確認
	competition, err := retrieveCompetition(ctx, tenantDB, competitionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "competition not found")
		}
		return fmt.Errorf("error retrieveCompetition: %w", err)
	}

	now := time.Now().Unix()
	var tenant TenantRow
	if err := adminDB.GetContext(ctx, &tenant, "SELECT `id` FROM tenant WHERE id = ?", v.tenantID); err != nil {
		return fmt.Errorf("error Select tenant: id=%d, %w", v.tenantID, err)
	}

	if _, err := adminDB.ExecContext(
		ctx,
		"INSERT INTO visit_history (player_id, tenant_id, competition_id, created_at) VALUES (?, ?, ?, ?)",
		v.playerID, tenant.ID, competitionID, now,
	); err != nil {
		var mysqlErr *mysql.MySQLError
		if !(errors.As(err, &mysqlErr) && mysqlErr.Number == 1062) {
			return fmt.Errorf(
				"error Insert visit_history: playerID=%s, tenantID=%d, competitionID=%s, createdAt=%d, updatedAt=%d, %w",
				v.playerID, tenant.ID, competitionID, now, now, err,
			)
		}
	} else {
		redisClient.client.Del(ctx, visitHistorySummaryCacheKey(competitionID))
	}

	var rankAfter int64
	rankAfterStr := c.QueryParam("rank_after")
	if rankAfterStr != "" {
		if rankAfter, err = strconv.ParseInt(rankAfterStr, 10, 64); err != nil {
			return fmt.Errorf("error strconv.ParseUint: rankAfterStr=%s, %w", rankAfterStr, err)
		}
	}

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	// fl, err := flockByTenantID(v.tenantID)
	// if err != nil {
	// 	return fmt.Errorf("error flockByTenantID: %w", err)
	// }
	// defer fl.Close()

	pss := []PlayerScoreRow{}

	if err := tenantDB.SelectContext(
		ctx,
		&pss,
		"SELECT score, player_id, row_num AS row_num FROM latest_player_score WHERE competition_id = ?",
		competitionID,
	); err != nil {
		return fmt.Errorf("error Select player_score: tenantID=%d, competitionID=%s, %w", tenant.ID, competitionID, err)
	}

	// pss, err := retrievePlayerScores(ctx, tenantDB, competitionID)
	// if err != nil {
	// 	return fmt.Errorf("error retrievePlayerScores: %w", err)
	// }

	ranks := make([]CompetitionRank, 0, len(pss))
	scoredPlayerSet := make(map[string]struct{}, len(pss))
	for _, ps := range pss {
		// player_scoreが同一player_id内ではrow_numの降順でソートされているので
		// 現れたのが2回目以降のplayer_idはより大きいrow_numでスコアが出ているとみなせる
		// if _, ok := scoredPlayerSet[ps.PlayerID]; ok {
		// 	continue
		// }
		scoredPlayerSet[ps.PlayerID] = struct{}{}
		p, err := retrievePlayer(ctx, tenantDB, ps.PlayerID)
		if err != nil {
			return fmt.Errorf("error retrievePlayer: %w", err)
		}
		ranks = append(ranks, CompetitionRank{
			Score:             ps.Score,
			PlayerID:          p.ID,
			PlayerDisplayName: p.DisplayName,
			RowNum:            ps.RowNum,
		})
	}
	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].Score == ranks[j].Score {
			return ranks[i].RowNum < ranks[j].RowNum
		}
		return ranks[i].Score > ranks[j].Score
	})
	pagedRanks := make([]CompetitionRank, 0, 100)
	for i, rank := range ranks {
		if int64(i) < rankAfter {
			continue
		}
		pagedRanks = append(pagedRanks, CompetitionRank{
			Rank:              int64(i + 1),
			Score:             rank.Score,
			PlayerID:          rank.PlayerID,
			PlayerDisplayName: rank.PlayerDisplayName,
		})
		if len(pagedRanks) >= 100 {
			break
		}
	}

	res := SuccessResult{
		Status: true,
		Data: CompetitionRankingHandlerResult{
			Competition: CompetitionDetail{
				ID:         competition.ID,
				Title:      competition.Title,
				IsFinished: competition.FinishedAt.Valid,
			},
			Ranks: pagedRanks,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type CompetitionsHandlerResult struct {
	Competitions []CompetitionDetail `json:"competitions"`
}

// 参加者向けAPI
// GET /api/player/competitions
// 大会の一覧を取得する
func playerCompetitionsHandler(c echo.Context) error {
	ctx := context.Background()

	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}
	return competitionsHandler(c, v, tenantDB)
}

// テナント管理者向けAPI
// GET /api/organizer/competitions
// 大会の一覧を取得する
func organizerCompetitionsHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	return competitionsHandler(c, v, tenantDB)
}

func competitionsHandler(c echo.Context, v *Viewer, tenantDB dbOrTx) error {
	ctx := context.Background()

	cs := []CompetitionRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&cs,
		"SELECT * FROM competition WHERE tenant_id=? ORDER BY created_at DESC",
		v.tenantID,
	); err != nil {
		return fmt.Errorf("error Select competition: %w", err)
	}
	cds := make([]CompetitionDetail, 0, len(cs))
	for _, comp := range cs {
		cds = append(cds, CompetitionDetail{
			ID:         comp.ID,
			Title:      comp.Title,
			IsFinished: comp.FinishedAt.Valid,
		})
	}

	res := SuccessResult{
		Status: true,
		Data: CompetitionsHandlerResult{
			Competitions: cds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type TenantDetail struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type MeHandlerResult struct {
	Tenant   *TenantDetail `json:"tenant"`
	Me       *PlayerDetail `json:"me"`
	Role     string        `json:"role"`
	LoggedIn bool          `json:"logged_in"`
}

// 共通API
// GET /api/me
// JWTで認証した結果、テナントやユーザ情報を返す
func meHandler(c echo.Context) error {
	tenant, err := retrieveTenantRowFromHeader(c)
	if err != nil {
		return fmt.Errorf("error retrieveTenantRowFromHeader: %w", err)
	}
	td := &TenantDetail{
		Name:        tenant.Name,
		DisplayName: tenant.DisplayName,
	}
	v, err := parseViewer(c)
	if err != nil {
		var he *echo.HTTPError
		if ok := errors.As(err, &he); ok && he.Code == http.StatusUnauthorized {
			return c.JSON(http.StatusOK, SuccessResult{
				Status: true,
				Data: MeHandlerResult{
					Tenant:   td,
					Me:       nil,
					Role:     RoleNone,
					LoggedIn: false,
				},
			})
		}
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role == RoleAdmin || v.role == RoleOrganizer {
		return c.JSON(http.StatusOK, SuccessResult{
			Status: true,
			Data: MeHandlerResult{
				Tenant:   td,
				Me:       nil,
				Role:     v.role,
				LoggedIn: true,
			},
		})
	}

	ctx := context.Background()
	p, err := retrievePlayer(ctx, tenantDB, v.playerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusOK, SuccessResult{
				Status: true,
				Data: MeHandlerResult{
					Tenant:   td,
					Me:       nil,
					Role:     RoleNone,
					LoggedIn: false,
				},
			})
		}
		return fmt.Errorf("error retrievePlayer: %w", err)
	}

	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data: MeHandlerResult{
			Tenant: td,
			Me: &PlayerDetail{
				ID:             p.ID,
				DisplayName:    p.DisplayName,
				IsDisqualified: p.IsDisqualified,
			},
			Role:     v.role,
			LoggedIn: true,
		},
	})
}

type InitializeHandlerResult struct {
	Lang string `json:"lang"`
}

// ベンチマーカー向けAPI
// POST /initialize
// ベンチマーカーが起動したときに最初に呼ぶ
// データベースの初期化などが実行されるため、スキーマを変更した場合などは適宜改変すること
func initializeHandler(c echo.Context) error {
	// delete all
	if err := redisClient.client.FlushDB(context.Background()).Err(); err != nil {
		return fmt.Errorf("error FlushDB: %w", err)
	}

	out, err := exec.Command(initializeScript).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error exec.Command: %s %e", string(out), err)
	}
	res := InitializeHandlerResult{
		Lang: "go",
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

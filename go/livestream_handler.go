package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
)

type ReserveLivestreamRequest struct {
	Tags         []int64 `json:"tags"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	PlaylistUrl  string  `json:"playlist_url"`
	ThumbnailUrl string  `json:"thumbnail_url"`
	StartAt      int64   `json:"start_at"`
	EndAt        int64   `json:"end_at"`
}

type LivestreamViewerModel struct {
	UserID       int64 `db:"user_id" json:"user_id"`
	LivestreamID int64 `db:"livestream_id" json:"livestream_id"`
	CreatedAt    int64 `db:"created_at" json:"created_at"`
}

type LivestreamModel struct {
	ID           int64  `db:"id" json:"id"`
	UserID       int64  `db:"user_id" json:"user_id"`
	Title        string `db:"title" json:"title"`
	Description  string `db:"description" json:"description"`
	PlaylistUrl  string `db:"playlist_url" json:"playlist_url"`
	ThumbnailUrl string `db:"thumbnail_url" json:"thumbnail_url"`
	StartAt      int64  `db:"start_at" json:"start_at"`
	EndAt        int64  `db:"end_at" json:"end_at"`
}

type LivestreamTagDBResult struct {
	LivestreamModel
	User *FullUserModel `db:"user"`
	Tag  *struct {
		ID   *int64  `db:"id"`
		Name *string `db:"name"`
	} `db:"tag"`
}

type FullLivestreamModel struct {
	ID           int64  `db:"id" json:"id"`
	UserID       int64  `db:"user_id" json:"user_id"`
	Title        string `db:"title" json:"title"`
	Description  string `db:"description" json:"description"`
	PlaylistUrl  string `db:"playlist_url" json:"playlist_url"`
	ThumbnailUrl string `db:"thumbnail_url" json:"thumbnail_url"`
	StartAt      int64  `db:"start_at" json:"start_at"`
	EndAt        int64  `db:"end_at" json:"end_at"`

	Tags []Tag `db:"tags" json:"tags"`
	User *User `db:"user" json:"user"`
}

type Livestream struct {
	ID           int64  `json:"id"`
	Owner        User   `json:"owner"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	PlaylistUrl  string `json:"playlist_url"`
	ThumbnailUrl string `json:"thumbnail_url"`
	Tags         []Tag  `json:"tags"`
	StartAt      int64  `json:"start_at"`
	EndAt        int64  `json:"end_at"`
}

type LivestreamTagModel struct {
	ID           int64 `db:"id" json:"id"`
	LivestreamID int64 `db:"livestream_id" json:"livestream_id"`
	TagID        int64 `db:"tag_id" json:"tag_id"`
}

type ReservationSlotModel struct {
	ID      int64 `db:"id" json:"id"`
	Slot    int64 `db:"slot" json:"slot"`
	StartAt int64 `db:"start_at" json:"start_at"`
	EndAt   int64 `db:"end_at" json:"end_at"`
}

func reserveLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	defer c.Request().Body.Close()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var req *ReserveLivestreamRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	// 2023/11/25 10:00からの１年間の期間内であるかチェック
	var (
		termStartAt    = time.Date(2023, 11, 25, 1, 0, 0, 0, time.UTC)
		termEndAt      = time.Date(2024, 11, 25, 1, 0, 0, 0, time.UTC)
		reserveStartAt = time.Unix(req.StartAt, 0)
		reserveEndAt   = time.Unix(req.EndAt, 0)
	)
	if (reserveStartAt.Equal(termEndAt) || reserveStartAt.After(termEndAt)) || (reserveEndAt.Equal(termStartAt) || reserveEndAt.Before(termStartAt)) {
		return echo.NewHTTPError(http.StatusBadRequest, "bad reservation time range")
	}

	// 予約枠をみて、予約が可能か調べる
	// NOTE: 並列な予約のoverbooking防止にFOR UPDATEが必要
	var slots []*ReservationSlotModel
	if err := tx.SelectContext(ctx, &slots, "SELECT * FROM reservation_slots WHERE start_at >= ? AND end_at <= ? FOR UPDATE", req.StartAt, req.EndAt); err != nil {
		c.Logger().Warnf("予約枠一覧取得でエラー発生: %+v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get reservation_slots: "+err.Error())
	}
	for _, slot := range slots {
		var count int
		if err := tx.GetContext(ctx, &count, "SELECT slot FROM reservation_slots WHERE start_at = ? AND end_at = ?", slot.StartAt, slot.EndAt); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get reservation_slots: "+err.Error())
		}
		c.Logger().Infof("%d ~ %d予約枠の残数 = %d\n", slot.StartAt, slot.EndAt, slot.Slot)
		if count < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("予約期間 %d ~ %dに対して、予約区間 %d ~ %dが予約できません", termStartAt.Unix(), termEndAt.Unix(), req.StartAt, req.EndAt))
		}
	}

	var (
		livestreamModel = &LivestreamModel{
			UserID:       int64(userID),
			Title:        req.Title,
			Description:  req.Description,
			PlaylistUrl:  req.PlaylistUrl,
			ThumbnailUrl: req.ThumbnailUrl,
			StartAt:      req.StartAt,
			EndAt:        req.EndAt,
		}
	)

	if _, err := tx.ExecContext(ctx, "UPDATE reservation_slots SET slot = slot - 1 WHERE start_at >= ? AND end_at <= ?", req.StartAt, req.EndAt); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to update reservation_slot: "+err.Error())
	}

	rs, err := tx.NamedExecContext(ctx, "INSERT INTO livestreams (user_id, title, description, playlist_url, thumbnail_url, start_at, end_at) VALUES(:user_id, :title, :description, :playlist_url, :thumbnail_url, :start_at, :end_at)", livestreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream: "+err.Error())
	}

	livestreamID, err := rs.LastInsertId()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get last inserted livestream id: "+err.Error())
	}
	livestreamModel.ID = livestreamID

	// タグ追加
	for _, tagID := range req.Tags {
		if _, err := tx.NamedExecContext(ctx, "INSERT INTO livestream_tags (livestream_id, tag_id) VALUES (:livestream_id, :tag_id)", &LivestreamTagModel{
			LivestreamID: livestreamID,
			TagID:        tagID,
		}); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream tag: "+err.Error())
		}
	}

	livestream, err := findLivestreamByIdInTx(ctx, tx, int(livestreamID))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to find livestream: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusCreated, livestream)
}

func searchLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	keyTagName := c.QueryParam("tag")

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var livestreamModels []*LivestreamModel
	if c.QueryParam("tag") != "" {
		// タグによる取得
		var tagIDList []int
		if err := tx.SelectContext(ctx, &tagIDList, "SELECT id FROM tags WHERE name = ?", keyTagName); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get tags: "+err.Error())
		}

		query, params, err := sqlx.In("SELECT * FROM livestream_tags WHERE tag_id IN (?) ORDER BY livestream_id DESC", tagIDList)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to construct IN query: "+err.Error())
		}
		var keyTaggedLivestreams []*LivestreamTagModel
		if err := tx.SelectContext(ctx, &keyTaggedLivestreams, query, params...); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get keyTaggedLivestreams: "+err.Error())
		}

		for _, keyTaggedLivestream := range keyTaggedLivestreams {
			ls := LivestreamModel{}
			if err := tx.GetContext(ctx, &ls, "SELECT * FROM livestreams WHERE id = ?", keyTaggedLivestream.LivestreamID); err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
			}

			livestreamModels = append(livestreamModels, &ls)
		}
	} else {
		// 検索条件なし
		query := `SELECT * FROM livestreams ORDER BY id DESC`
		if c.QueryParam("limit") != "" {
			limit, err := strconv.Atoi(c.QueryParam("limit"))
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "limit query parameter must be integer")
			}
			query += fmt.Sprintf(" LIMIT %d", limit)
		}

		if err := tx.SelectContext(ctx, &livestreamModels, query); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
		}
	}

	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		livestream, err := findLivestreamByIdInTx(ctx, tx, int(livestreamModels[i].ID))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
		}
		livestreams[i] = livestream
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestreams)
}

func getMyLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	var livestreamModels []*LivestreamModel
	if err := tx.SelectContext(ctx, &livestreamModels, "SELECT * FROM livestreams WHERE user_id = ?", userID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}
	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		livestream, err := findLivestreamByIdInTx(ctx, tx, int(livestreamModels[i].ID))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
		}
		livestreams[i] = livestream
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestreams)
}

func getUserLivestreamsHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}

	username := c.Param("username")

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var user UserModel
	if err := tx.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "user not found")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
		}
	}

	var livestreamModels []*LivestreamModel
	if err := tx.SelectContext(ctx, &livestreamModels, "SELECT * FROM livestreams WHERE user_id = ?", user.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}
	livestreams := make([]Livestream, len(livestreamModels))
	for i := range livestreamModels {
		livestream, err := findLivestreamByIdInTx(ctx, tx, int(livestreamModels[i].ID))
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
		}
		livestreams[i] = livestream
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestreams)
}

// viewerテーブルの廃止
func enterLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id must be integer")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	viewer := LivestreamViewerModel{
		UserID:       int64(userID),
		LivestreamID: int64(livestreamID),
		CreatedAt:    time.Now().Unix(),
	}

	if _, err := tx.NamedExecContext(ctx, "INSERT INTO livestream_viewers_history (user_id, livestream_id, created_at) VALUES(:user_id, :livestream_id, :created_at)", viewer); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to insert livestream_view_history: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

func exitLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM livestream_viewers_history WHERE user_id = ? AND livestream_id = ?", userID, livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to delete livestream_view_history: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.NoContent(http.StatusOK)
}

func getLivestreamHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	livestream, err := findLivestreamByIdInTx(ctx, dbConn, livestreamID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
	}

	return c.JSON(http.StatusOK, livestream)
}

func getLivecommentReportsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var livestreamModel LivestreamModel
	if err := tx.GetContext(ctx, &livestreamModel, "SELECT * FROM livestreams WHERE id = ?", livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream: "+err.Error())
	}

	// error already check
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already check
	userID := sess.Values[defaultUserIDKey].(int64)

	if livestreamModel.UserID != userID {
		return echo.NewHTTPError(http.StatusForbidden, "can't get other streamer's livecomment reports")
	}

	var reportModels []*LivecommentReportModel
	if err := tx.SelectContext(ctx, &reportModels, "SELECT * FROM livecomment_reports WHERE livestream_id = ?", livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livecomment reports: "+err.Error())
	}

	reports := make([]LivecommentReport, len(reportModels))
	for i := range reportModels {
		report, err := fillLivecommentReportResponse(ctx, tx, *reportModels[i])
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livecomment report: "+err.Error())
		}
		reports[i] = report
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, reports)
}

func findLivestreamByIdInTx(ctx context.Context, db myDB, lsId int) (Livestream, error) {
	var dbResult []*LivestreamTagDBResult
	err := db.SelectContext(ctx, &dbResult,
		`
		SELECT 
			ls.*,
			u.id as "user.id",
			u.name as "user.name",
			u.display_name as "user.display_name",
			u.description as "user.description",
			u.password as "user.password",
			t.id as "tag.id",
			t.name as "tag.name",
			te.id AS 'user.theme.id',
			te.dark_mode AS 'user.theme.dark_mode',
			i.image AS 'user.user_image'
		FROM livestreams ls
		LEFT JOIN users u ON ls.user_id = u.id
		LEFT JOIN themes te ON u.id = te.user_id
		LEFT JOIN icons i ON u.id = i.user_id
		LEFT JOIN livestream_tags lst ON ls.id = lst.livestream_id
		LEFT JOIN tags t ON lst.tag_id = t.id
		WHERE ls.id = ?
		`,
		lsId)
	if err != nil {
		return Livestream{}, echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
	}

	dummy, err := os.ReadFile(fallbackImage)
	if err != nil {
		return Livestream{}, nil
	}

	var fullLivestreamModel *FullLivestreamModel
	for _, result := range dbResult {
		if fullLivestreamModel == nil {
			fullLivestreamModel = &FullLivestreamModel{
				ID:           result.ID,
				UserID:       result.UserID,
				Title:        result.Title,
				Description:  result.Description,
				PlaylistUrl:  result.PlaylistUrl,
				ThumbnailUrl: result.ThumbnailUrl,
				StartAt:      result.StartAt,
				EndAt:        result.EndAt,
				Tags:         make([]Tag, 0),
			}

			var image []byte
			image = result.User.UserImage
			if image == nil {
				image = dummy
			}

			iconHash := sha256.Sum256(image)

			if result.User != nil {
				fullLivestreamModel.User = &User{
					ID:          result.User.ID,
					Name:        result.User.Name,
					DisplayName: result.User.DisplayName,
					Description: result.User.Description,
					Theme: Theme{
						ID:       result.User.Theme.ID,
						DarkMode: result.User.Theme.DarkMode,
					},
					IconHash: fmt.Sprintf("%x", iconHash),
				}
			}
		}
		if result.Tag != nil && result.Tag.ID != nil && result.Tag.Name != nil {
			fullLivestreamModel.Tags = append(fullLivestreamModel.Tags, Tag{
				ID:   lo.FromPtr(result.Tag.ID),
				Name: lo.FromPtr(result.Tag.Name),
			})
		}
	}

	livestream := Livestream{
		ID:           fullLivestreamModel.ID,
		Owner:        *fullLivestreamModel.User,
		Title:        fullLivestreamModel.Title,
		Description:  fullLivestreamModel.Description,
		PlaylistUrl:  fullLivestreamModel.PlaylistUrl,
		ThumbnailUrl: fullLivestreamModel.ThumbnailUrl,
		StartAt:      fullLivestreamModel.StartAt,
		EndAt:        fullLivestreamModel.EndAt,
		Tags:         fullLivestreamModel.Tags,
	}

	return livestream, nil
}

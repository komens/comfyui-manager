package main

import (
	"context"
	"database/sql"
)

// 图片与提示词的收藏是关联的：以提示词为收藏主体，图片的收藏态跟随其所属提示词。
// - 收藏/取消一张图片  → 同步其所属提示词的收藏态（并级联到该提示词名下所有图片）
// - 收藏/取消一个提示词 → 级联更新该提示词名下所有图片
// 手动提交、无关联提示词的图片（prompt_id=0）只影响自身。

// setPromptFavorite 设置提示词收藏态，并级联到其名下所有结果图。fav: 1/0。
func (a *app) setPromptFavorite(ctx context.Context, promptID int64, fav int) error {
	res, err := a.db.ExecContext(ctx, `UPDATE prompts SET is_favorite=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, fav, promptID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errPromptNotFound
	}
	_, err = a.db.ExecContext(ctx,
		`UPDATE images SET is_favorite=? WHERE generation_item_id IN (SELECT id FROM generation_items WHERE prompt_id=?)`, fav, promptID)
	return err
}

// promptIDForImage 返回图片关联的提示词 id；手动提交（无关联）返回 0。
func (a *app) promptIDForImage(ctx context.Context, imageID int64) (int64, error) {
	var promptID sql.NullInt64
	err := a.db.QueryRowContext(ctx,
		`SELECT gi.prompt_id FROM images i JOIN generation_items gi ON i.generation_item_id=gi.id WHERE i.id=?`, imageID).Scan(&promptID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, errImageNotFound
		}
		return 0, err
	}
	return promptID.Int64, nil
}

// setImageFavoriteLinked 设置单张图片收藏态；有关联提示词时以提示词为准并级联，
// 无关联（手动提交）时只改这张图。
func (a *app) setImageFavoriteLinked(ctx context.Context, imageID int64, fav int) error {
	promptID, err := a.promptIDForImage(ctx, imageID)
	if err != nil {
		return err
	}
	if promptID > 0 {
		return a.setPromptFavorite(ctx, promptID, fav)
	}
	return a.setImageFavoriteByID(ctx, imageID, fav)
}

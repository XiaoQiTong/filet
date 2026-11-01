package com.xiaoqi.filet;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.content.Context;
import android.database.Cursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;

import java.io.File;
import java.io.FileNotFoundException;
import java.io.IOException;

/**
 * 把「检测到新版本」下载下来的 APK 以 content:// 交给系统安装器。
 * Android 7 起 file:// 递给别的进程会被拒，而 AndroidX 的 FileProvider 又不想为一个壳工程引进来。
 */
public class ApkProvider extends ContentProvider {

    private static final String AUTHORITY = "com.xiaoqi.filet.apk";
    private static final String DIR_NAME = "update";

    static File dir(Context ctx) {
        File external = ctx.getExternalFilesDir(DIR_NAME);
        return external == null ? new File(ctx.getFilesDir(), DIR_NAME) : external;
    }

    static Uri uriFor(File file) {
        return new Uri.Builder().scheme("content").authority(AUTHORITY).appendPath(file.getName()).build();
    }

    @Override
    public boolean onCreate() {
        return true;
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        if (mode != null && !"r".equals(mode)) {
            throw new FileNotFoundException("这个目录只读");
        }
        File base = dir(getContext());
        File file = new File(base, String.valueOf(uri.getLastPathSegment()));
        try {
            String canonical = file.getCanonicalPath();
            if (!canonical.startsWith(base.getCanonicalPath() + File.separator)
                    || !canonical.endsWith(".apk") || !file.isFile()) {
                throw new FileNotFoundException("不提供这个文件");
            }
            return ParcelFileDescriptor.open(file, ParcelFileDescriptor.MODE_READ_ONLY);
        } catch (IOException err) {
            throw new FileNotFoundException("读不到文件：" + err.getMessage());
        }
    }

    @Override
    public String getType(Uri uri) {
        return "application/vnd.android.package-archive";
    }

    @Override
    public Cursor query(Uri uri, String[] projection, String selection, String[] selectionArgs,
                        String sortOrder) {
        throw new UnsupportedOperationException();
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        throw new UnsupportedOperationException();
    }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException();
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException();
    }
}

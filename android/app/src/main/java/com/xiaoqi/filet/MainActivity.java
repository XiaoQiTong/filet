package com.xiaoqi.filet;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.ContentResolver;
import android.content.ContentValues;
import android.content.Intent;
import android.content.SharedPreferences;
import android.database.Cursor;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.PowerManager;
import android.provider.MediaStore;
import android.provider.OpenableColumns;
import android.util.Base64;
import android.view.ViewGroup;
import android.webkit.JavascriptInterface;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;

/**
 * filet 的 Android 壳：加载自己服务端的网页，另外补三件 WebView 自己做不到的事——
 * 1) 注册系统「打开方式」，把微信传来的文件喂给网页去发；
 * 2) 接收到的文件写进手机下载目录（WebView 里 blob 下载点了没反应）；
 * 3) 记住服务器地址，传输期间持有 WakeLock。
 */
public class MainActivity extends Activity {

    private static final String PREFS = "filet";
    private static final String PREF_SERVER = "server";
    private static final int REQ_FILE = 1001;

    // 文件是整份读进 JS 内存再发的（网页没有流式读 content:// 的办法），所以设上限
    private static final long MAX_PUSH_BYTES = 128L * 1024 * 1024;
    private static final int PUSH_CHUNK_BYTES = 192 * 1024;
    private static final int PUSH_WINDOW = 4;

    // 只在自己 GitHub 仓的 Release 里找新版本；拿不到就当没更新
    private static final String RELEASE_API = "https://api.github.com/repos/XiaoQiTong/filet/releases/latest";
    private static final String PREF_UPDATE_AT = "updateCheckAt";
    private static final long UPDATE_CHECK_INTERVAL = 24 * 3600 * 1000L;

    private WebView web;
    private PowerManager.WakeLock wakeLock;
    private SharedPreferences prefs;
    private final Map<String, File> saveFiles = new HashMap<>();
    private final Map<String, OutputStream> saveStreams = new HashMap<>();
    private final Map<String, String> saveNames = new HashMap<>();

    private ValueCallback<Uri[]> filePathCallback;
    private String incomingName;
    private Uri incomingUri;
    private boolean pageReady;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        prefs = getSharedPreferences(PREFS, MODE_PRIVATE);

        web = new WebView(this);
        setContentView(web, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        WebSettings settings = web.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setAllowContentAccess(true);
        // 桥在网页 app.js 里，服务端升级后必须拿到新版页面，不能被 WebView 缓存挡住
        settings.setCacheMode(WebSettings.LOAD_NO_CACHE);
        if ((getApplicationInfo().flags & android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            WebView.setWebContentsDebuggingEnabled(true);
        }
        web.setWebViewClient(new Client());
        web.setWebChromeClient(new Chooser());
        web.addJavascriptInterface(new Bridge(), "AndroidFileBridge");

        PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "filet:transfer");
        wakeLock.setReferenceCounted(false);

        handleIntent(getIntent());
        String server = prefs.getString(PREF_SERVER, "");
        if (server == null || server.isEmpty()) {
            askServer(true);
        } else {
            web.loadUrl(server);
        }
        if (incomingUri == null && server != null && !server.isEmpty()) {
            maybeCheckUpdate();
        }
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        handleIntent(intent);
    }

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) {
            web.goBack();
            return;
        }
        super.onBackPressed();
    }

    @Override
    protected void onDestroy() {
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
        for (OutputStream os : saveStreams.values()) {
            try {
                os.close();
            } catch (Exception ignored) {
                // 反正进程要没了
            }
        }
        saveStreams.clear();
        super.onDestroy();
    }

    // ---------------------------------------------------------------- 外部文件

    private void handleIntent(Intent intent) {
        if (intent == null || incomingUri != null) {
            return;
        }
        Uri data = null;
        if (Intent.ACTION_SEND.equals(intent.getAction())) {
            Object extra = intent.getParcelableExtra(Intent.EXTRA_STREAM);
            if (extra instanceof Uri) {
                data = (Uri) extra;
            }
        } else if (Intent.ACTION_VIEW.equals(intent.getAction())) {
            data = intent.getData();
        }
        if (data == null) {
            return;
        }
        long size = querySize(data);
        if (size > MAX_PUSH_BYTES) {
            toast("文件 " + (size / 1024 / 1024) + "MB 超过 128MB，请在电脑上用 filet 发送");
            return;
        }
        incomingUri = data;
        incomingName = queryName(data);
        maybePush();
    }

    /** 原生没有流式喂 DataChannel 的口子，只能整份 base64 推给网页拼成 File。 */
    private synchronized void maybePush() {
        if (incomingUri == null || web == null || !pageReady) {
            return;
        }
        final Uri uri = incomingUri;
        final String name = incomingName;
        incomingUri = null;
        new Thread(() -> {
            try (InputStream in = getContentResolver().openInputStream(uri)) {
                if (in == null) {
                    toast("读不到这个文件，请在 filet 里手动选一次");
                    return;
                }
                js("window.FiletAndroid&&FiletAndroid.fileStart(" + JSONObject.quote(name) + ")");
                byte[] buf = new byte[PUSH_CHUNK_BYTES];
                Semaphore window = new Semaphore(PUSH_WINDOW);
                long sent = 0;
                int n;
                while ((n = in.read(buf)) > 0) {
                    sent += n;
                    if (sent > MAX_PUSH_BYTES) {
                        js("window.FiletAndroid&&FiletAndroid.fileAbort()");
                        toast("文件超过 128MB，已停止（请用电脑端 filet 发送）");
                        return;
                    }
                    final String b64 = Base64.encodeToString(buf, 0, n, Base64.NO_WRAP);
                    if (!window.tryAcquire(30, TimeUnit.SECONDS)) {
                        js("window.FiletAndroid&&FiletAndroid.fileAbort()");
                        toast("网页没有继续接收，已停止");
                        return;
                    }
                    runOnUiThread(() -> web.evaluateJavascript(
                            "window.FiletAndroid&&FiletAndroid.fileChunk('" + b64 + "')",
                            value -> window.release()));
                }
                js("window.FiletAndroid&&FiletAndroid.fileEnd()");
            } catch (Exception err) {
                js("window.FiletAndroid&&FiletAndroid.fileAbort()");
                toast("读取文件失败：" + describe(err));
            }
        }).start();
    }

    private void js(String script) {
        runOnUiThread(() -> web.evaluateJavascript(script, null));
    }

    private String queryName(Uri uri) {
        if (ContentResolver.SCHEME_CONTENT.equals(uri.getScheme())) {
            Cursor c = null;
            try {
                c = getContentResolver().query(uri, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null);
                if (c != null && c.moveToFirst()) {
                    int idx = c.getColumnIndex(OpenableColumns.DISPLAY_NAME);
                    if (idx >= 0) {
                        String got = c.getString(idx);
                        if (got != null && !got.isEmpty()) {
                            return got;
                        }
                    }
                }
            } catch (Exception ignored) {
                // 拿不到名字就用路径兜底
            } finally {
                if (c != null) {
                    c.close();
                }
            }
        }
        String last = uri.getLastPathSegment();
        if (last == null || last.isEmpty()) {
            return "filet-" + System.currentTimeMillis() + ".bin";
        }
        return new File(last).getName();
    }

    private long querySize(Uri uri) {
        if (!ContentResolver.SCHEME_CONTENT.equals(uri.getScheme())) {
            File f = new File(uri.getPath() == null ? "" : uri.getPath());
            return f.isFile() ? f.length() : -1;
        }
        Cursor c = null;
        try {
            c = getContentResolver().query(uri, new String[]{OpenableColumns.SIZE}, null, null, null);
            if (c != null && c.moveToFirst()) {
                int idx = c.getColumnIndex(OpenableColumns.SIZE);
                if (idx >= 0 && !c.isNull(idx)) {
                    return c.getLong(idx);
                }
            }
        } catch (Exception ignored) {
            // 大小未知，交给后面的读取上限兜底
        } finally {
            if (c != null) {
                c.close();
            }
        }
        return -1;
    }

    // ---------------------------------------------------------------- 服务器地址

    private void askServer(boolean first) {
        EditText input = new EditText(this);
        input.setHint("http://1.2.3.4:8085 或 https://files.example.com");
        input.setInputType(android.text.InputType.TYPE_CLASS_TEXT
                | android.text.InputType.TYPE_TEXT_VARIATION_URI);
        String saved = prefs.getString(PREF_SERVER, "");
        if (saved != null && !saved.isEmpty()) {
            input.setText(saved);
            input.setSelection(input.getText().length());
        }
        AlertDialog.Builder builder = new AlertDialog.Builder(this)
                .setTitle(first ? "填写 filet 服务器地址" : "更改服务器地址")
                .setView(input)
                .setPositiveButton("连接", (dialog, which) -> {
                    String url = normalize(input.getText().toString().trim());
                    if (url == null) {
                        toast("地址不对，要带端口或域名，例如 http://1.2.3.4:8085");
                        if (first) {
                            askServer(true);
                        }
                        return;
                    }
                    prefs.edit().putString(PREF_SERVER, url).apply();
                    pageReady = false;
                    web.loadUrl(url);
                });
        if (!first) {
            builder.setNegativeButton("取消", null);
        }
        builder.show();
    }

    private static String normalize(String raw) {
        if (raw.isEmpty()) {
            return null;
        }
        String url = raw.startsWith("http://") || raw.startsWith("https://")
                ? raw
                : "http://" + raw;
        Uri parsed = Uri.parse(url);
        if (parsed.getHost() == null || parsed.getHost().isEmpty()) {
            return null;
        }
        if (parsed.getPath() != null && !parsed.getPath().equals("/")) {
            url = url.substring(0, url.length() - parsed.getPath().length());
        }
        return url;
    }

    private String currentServer() {
        return prefs.getString(PREF_SERVER, "");
    }

    // ---------------------------------------------------------------- 网页桥

    private class Bridge {

        @JavascriptInterface
        public void pageReady() {
            pageReady = true;
            maybePush();
        }

        @JavascriptInterface
        public void editServer() {
            runOnUiThread(() -> askServer(false));
        }

        @JavascriptInterface
        public void setBusy(boolean busy) {
            if (busy) {
                if (!wakeLock.isHeld()) {
                    wakeLock.acquire(4 * 60 * 60 * 1000L);
                }
            } else if (wakeLock.isHeld()) {
                wakeLock.release();
            }
        }

        @JavascriptInterface
        public synchronized String openSave(String name) {
            String token = Long.toString(System.nanoTime());
            File tmp = new File(getCacheDir(), "filet-save-" + token + ".tmp");
            try {
                saveStreams.put(token, new FileOutputStream(tmp));
                saveFiles.put(token, tmp);
                saveNames.put(token, sanitizeName(name));
            } catch (Exception err) {
                toast("无法创建接收文件：" + describe(err));
            }
            return token;
        }

        @JavascriptInterface
        public synchronized void writeSave(String token, String base64) {
            OutputStream os = saveStreams.get(token);
            if (os == null) {
                return;
            }
            try {
                os.write(Base64.decode(base64, Base64.NO_WRAP));
            } catch (Exception err) {
                toast("写入文件失败：" + describe(err));
                abortSave(token);
            }
        }

        @JavascriptInterface
        public synchronized String closeSave(String token) {
            File tmp = saveFiles.remove(token);
            OutputStream os = saveStreams.remove(token);
            String name = saveNames.remove(token);
            closeQuietly(os);
            if (tmp == null || !tmp.isFile()) {
                return "";
            }
            if (name == null) {
                name = "filet-" + System.currentTimeMillis() + ".bin";
            }
            // 优先写系统下载目录；失败就退回应用专属目录，至少别把收到的数据丢掉
            String dest;
            try {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                    dest = saveToDownloadsQ(name, tmp);
                } else {
                    dest = saveToDownloadsLegacy(name, tmp);
                }
                tmp.delete();
            } catch (Exception err) {
                String fallback = null;
                try {
                    fallback = saveToDownloadsLegacy(name, tmp);
                } catch (Exception ignored) {
                    // 应用目录也写不了，只能返回临时路径
                }
                if (fallback != null) {
                    toast("下载目录写不了（" + describe(err) + "），已存到应用目录");
                    dest = fallback;
                    tmp.delete();
                } else {
                    toast("保存失败：" + describe(err));
                    dest = tmp.getAbsolutePath();
                }
            }
            return dest;
        }

        @JavascriptInterface
        public synchronized void abortSave(String token) {
            closeQuietly(saveStreams.remove(token));
            saveNames.remove(token);
            File tmp = saveFiles.remove(token);
            if (tmp != null) {
                tmp.delete();
            }
        }
    }

    private String saveToDownloadsQ(String name, File tmp) throws Exception {
        ContentValues values = new ContentValues();
        values.put(MediaStore.MediaColumns.DISPLAY_NAME, name);
        values.put(MediaStore.MediaColumns.MIME_TYPE, "application/octet-stream");
        values.put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/filet");
        Uri target = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values);
        if (target == null) {
            throw new IllegalStateException("MediaStore 拒绝写入");
        }
        try (OutputStream os = getContentResolver().openOutputStream(target)) {
            copy(tmp, os);
        }
        return Environment.DIRECTORY_DOWNLOADS + "/filet/" + name;
    }

    private String saveToDownloadsLegacy(String name, File tmp) throws Exception {
        File dir = getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS);
        File out = uniqueFile(dir, name);
        try (OutputStream os = new FileOutputStream(out)) {
            copy(tmp, os);
        }
        return out.getAbsolutePath();
    }

    private static File uniqueFile(File dir, String name) {
        File out = new File(dir, name);
        int dot = name.lastIndexOf('.');
        String base = dot > 0 ? name.substring(0, dot) : name;
        String ext = dot > 0 ? name.substring(dot) : "";
        int i = 1;
        while (out.exists() && i < 1000) {
            out = new File(dir, base + " (" + i + ")" + ext);
            i++;
        }
        return out;
    }

    private static String sanitizeName(String raw) {
        String name = raw.replaceAll("[\\\\/:*?\"<>|]", "_").replaceAll("\\.\\.", "_");
        if (name.isEmpty()) {
            return "filet.bin";
        }
        return name.length() > 120 ? name.substring(name.length() - 120) : name;
    }

    // ---------------------------------------------------------------- 通用

    private static void copy(File from, OutputStream to) throws Exception {
        byte[] buf = new byte[64 * 1024];
        try (InputStream in = new FileInputStream(from)) {
            int n;
            while ((n = in.read(buf)) > 0) {
                to.write(buf, 0, n);
            }
            to.flush();
        }
    }

    private static void closeQuietly(OutputStream os) {
        if (os == null) {
            return;
        }
        try {
            os.close();
        } catch (Exception ignored) {
            // 已经尽力了
        }
    }

    private static String describe(Exception err) {
        String msg = err.getMessage();
        return msg == null || msg.isEmpty() ? err.getClass().getSimpleName() : msg;
    }

    private void toast(String text) {
        runOnUiThread(() -> Toast.makeText(MainActivity.this, text, Toast.LENGTH_LONG).show());
    }

    // ---------------------------------------------------------------- 版本更新

    /** 问一次 GitHub 最新 Release，拿不到、超时、被墙都当没更新，不弹任何东西打扰。 */
    private void maybeCheckUpdate() {
        long now = System.currentTimeMillis();
        if (now - prefs.getLong(PREF_UPDATE_AT, 0L) < UPDATE_CHECK_INTERVAL) {
            return;
        }
        prefs.edit().putLong(PREF_UPDATE_AT, now).apply();
        new Thread(() -> {
            String tag = "";
            String notes = "";
            String url = "";
            long size = 0;
            try {
                JSONObject release = new JSONObject(httpGet(RELEASE_API, 8000));
                tag = release.optString("tag_name");
                JSONArray assets = release.optJSONArray("assets");
                for (int i = 0; assets != null && i < assets.length(); i++) {
                    JSONObject asset = assets.optJSONObject(i);
                    if (asset != null && "filet-android.apk".equals(asset.optString("name"))) {
                        url = asset.optString("browser_download_url");
                        size = asset.optLong("size");
                        notes = release.optString("body");
                    }
                }
            } catch (Exception ignored) {
                return;
            }
            if (url.isEmpty() || !isNewer(tag, installedVersion())) {
                return;
            }
            final String fTag = tag;
            final String fNotes = notes;
            final String fUrl = url;
            final long fSize = size;
            runOnUiThread(() -> askInstall(fTag, fNotes, fUrl, fSize));
        }).start();
    }

    private void askInstall(String tag, String notes, final String url, long size) {
        String kb = size > 0 ? "（" + Math.max(1, size / 1024) + "KB）" : "";
        new AlertDialog.Builder(this)
                .setTitle("filet " + tag + " 可更新" + kb)
                .setMessage(summary(notes))
                .setPositiveButton("下载安装", (dialog, which) -> downloadAndInstall(url, tag))
                .setNegativeButton("以后再说", null)
                .show();
    }

    private void downloadAndInstall(final String url, String tag) {
        new Thread(() -> {
            HttpURLConnection conn = null;
            File out = new File(ApkProvider.dir(MainActivity.this), "filet-" + tag.replaceAll("[^0-9A-Za-z._-]", "") + ".apk");
            try {
                for (File stale : emptyIfNull(out.getParentFile().listFiles())) {
                    if (stale.getName().endsWith(".apk")) {
                        stale.delete();
                    }
                }
                conn = (HttpURLConnection) new URL(url).openConnection();
                conn.setConnectTimeout(15000);
                conn.setReadTimeout(60000);
                if (conn.getResponseCode() != 200) {
                    toast("下载失败：HTTP " + conn.getResponseCode());
                    return;
                }
                try (InputStream in = conn.getInputStream(); OutputStream os = new FileOutputStream(out)) {
                    byte[] buf = new byte[16384];
                    int n;
                    while ((n = in.read(buf)) > 0) {
                        os.write(buf, 0, n);
                    }
                    os.flush();
                }
                installApk(out);
            } catch (Exception err) {
                out.delete();
                toast("下载失败：" + describe(err));
            } finally {
                if (conn != null) {
                    conn.disconnect();
                }
            }
        }).start();
    }

    private void installApk(File file) {
        Intent intent = new Intent(Intent.ACTION_VIEW);
        intent.setDataAndType(ApkProvider.uriFor(file), "application/vnd.android.package-archive");
        intent.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        try {
            startActivity(intent);
        } catch (Exception err) {
            toast("打不开安装界面：" + describe(err));
        }
    }

    private String installedVersion() {
        try {
            return getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
        } catch (Exception err) {
            return "";
        }
    }

    private static String httpGet(String url, int timeoutMs) throws IOException {
        HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
        conn.setRequestProperty("User-Agent", "filet-android");
        conn.setRequestProperty("Accept", "application/vnd.github+json");
        conn.setConnectTimeout(timeoutMs);
        conn.setReadTimeout(timeoutMs);
        try {
            if (conn.getResponseCode() != 200) {
                throw new IOException("HTTP " + conn.getResponseCode());
            }
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[4096];
            int n;
            int total = 0;
            try (InputStream in = conn.getInputStream()) {
                while ((n = in.read(buf)) > 0 && total < 256 * 1024) {
                    bos.write(buf, 0, n);
                    total += n;
                }
            }
            return new String(bos.toByteArray(), "UTF-8");
        } finally {
            conn.disconnect();
        }
    }

    private static String summary(String body) {
        String text = body == null ? "" : body.replaceAll("[#*`]", "").trim();
        if (text.isEmpty()) {
            return "去 GitHub 看更新说明。";
        }
        return text.length() > 240 ? text.substring(0, 240) + "…" : text;
    }

    private static File[] emptyIfNull(File[] files) {
        return files == null ? new File[0] : files;
    }

    static boolean isNewer(String want, String have) {
        int[] a = parseVersion(want);
        int[] b = parseVersion(have);
        for (int i = 0; i < Math.max(a.length, b.length); i++) {
            int x = i < a.length ? a[i] : 0;
            int y = i < b.length ? b[i] : 0;
            if (x != y) {
                return x > y;
            }
        }
        return false;
    }

    private static int[] parseVersion(String raw) {
        String text = raw == null ? "" : raw.trim();
        if (text.startsWith("v")) {
            text = text.substring(1);
        }
        String[] parts = text.isEmpty() ? new String[0] : text.split("\\.");
        int[] out = new int[parts.length];
        for (int i = 0; i < parts.length; i++) {
            String digits = parts[i].replaceAll("\\D.*$", "");
            try {
                out[i] = Integer.parseInt(digits);
            } catch (Exception err) {
                out[i] = 0;
            }
        }
        return out;
    }

    // ---------------------------------------------------------------- WebView 回调

    private class Client extends WebViewClient {

        @Override
        public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
            Uri base = Uri.parse(currentServer());
            Uri to = request.getUrl();
            boolean sameHost = base.getHost() != null && base.getHost().equals(to.getHost())
                    && (base.getPort() == to.getPort());
            if (sameHost) {
                return false;
            }
            try {
                startActivity(new Intent(Intent.ACTION_VIEW, to));
            } catch (Exception ignored) {
                // 没有能接的应用就算了
            }
            return true;
        }

        @Override
        public void onPageFinished(WebView view, String url) {
            // 旧版服务端的 app.js 里没有桥，文件会静静躺着不动——探测一次，说清楚原因
            view.evaluateJavascript(
                    "(typeof window.FiletAndroid==='object'&&typeof window.AndroidFileBridge==='object')?1:0",
                    value -> {
                        boolean supported = !"0".equals(value);
                        if (!supported && incomingUri != null) {
                            toast("服务端网页还是旧版本，没有 Android 桥；请把 filet 升级到 v0.4.0 再从微信打开");
                        }
                    });
        }

        @Override
        public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
            if (!request.isForMainFrame()) {
                return;
            }
            toast("连不上 " + currentServer() + "，请检查地址或服务端");
            runOnUiThread(() -> askServer(false));
        }
    }

    private class Chooser extends WebChromeClient {

        @Override
        public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback,
                                         FileChooserParams params) {
            if (filePathCallback != null) {
                callback.onReceiveValue(null);
                return false;
            }
            filePathCallback = callback;
            try {
                Intent pick = new Intent(Intent.ACTION_OPEN_DOCUMENT);
                pick.addCategory(Intent.CATEGORY_OPENABLE);
                pick.setType("*/*");
                startActivityForResult(pick, REQ_FILE);
            } catch (Exception err) {
                filePathCallback = null;
                toast("打不开系统文件选择器：" + describe(err));
                callback.onReceiveValue(null);
                return false;
            }
            return true;
        }
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode != REQ_FILE || filePathCallback == null) {
            return;
        }
        ValueCallback<Uri[]> cb = filePathCallback;
        filePathCallback = null;
        Uri got = data == null ? null : data.getData();
        cb.onReceiveValue(got == null ? null : new Uri[]{got});
    }
}

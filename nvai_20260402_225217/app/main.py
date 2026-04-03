from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import RedirectResponse
from fastapi.staticfiles import StaticFiles

from app.api.admin import router as admin_router
from app.api.openai_compat import router as openai_router
from app.config import settings
from app.key_health import start_key_probe_scheduler, stop_key_probe_scheduler
from app.key_manager import key_manager
from app.model_manager import model_manager
from app.proxy import close_client


@asynccontextmanager
async def lifespan(app: FastAPI):
    key_manager.load()
    model_manager.load()
    await start_key_probe_scheduler()
    yield
    await stop_key_probe_scheduler()
    await close_client()


app = FastAPI(title=settings.APP_NAME, debug=settings.DEBUG, lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.CORS_ORIGINS,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(openai_router)
app.include_router(admin_router)

web_dir = Path(__file__).resolve().parent.parent / "web"
if web_dir.is_dir():
    app.mount("/web", StaticFiles(directory=str(web_dir), html=True), name="web")


@app.get("/")
async def root():
    return RedirectResponse(url="/web/")


@app.get("/health")
async def health():
    return {"status": "ok"}

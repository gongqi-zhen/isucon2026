import asyncio

from fastapi import APIRouter

from ..schemas import InitializeResponse

router = APIRouter()


@router.post("/api/initialize")
async def initialize() -> InitializeResponse:
    process = await asyncio.create_subprocess_exec("../sql/init.sh")
    if await process.wait() != 0:
        raise RuntimeError(f"exit status {process.returncode}")
    # Set language to the implementation language.
    # Agents optimizing this app: set agent, harness, and model to the names used.
    # For multiple models, use comma-separated names; use brain when no AI model is used.
    return InitializeResponse(
        language="python", agent="human", harness="manual", model="brain"
    )
